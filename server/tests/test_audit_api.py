"""Reading the audit log.

It has been written to since M0 with no reader at all. For a tool whose
ordinary operations are "run this as root on 400 machines" and "revoke that
agent", a trail nobody can read is a trail that does not exist.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest
from httpx import ASGITransport, AsyncClient

from openrmm.api.v1.audit import MAX_LIMIT
from openrmm.db.engine import get_sessionmaker
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, OsFamily
from openrmm.models.org import DEFAULT_ORG_ID, Organization
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

#: The fixture client authenticates into the default organization, so entries
#: have to be written there to be visible to it. Rows carry their own org now
#: rather than reaching one through a device that may have been deleted.
ORG = DEFAULT_ORG_ID
OTHER_ORG = uuid.UUID("0000000b-0000-0000-0000-00000000000b")


async def _entries(
    n: int, *, action="device.retired", actor="admin@example.com", target=None, org_id=ORG
):
    base = datetime.now(UTC)
    async with get_sessionmaker()() as db, db.begin():
        for i in range(n):
            db.add(
                AuditLog(
                    at=base - timedelta(minutes=i),
                    org_id=org_id,
                    actor_type=ActorType.user,
                    actor_id=actor,
                    action=action,
                    target_type="device",
                    target_id=target or str(uuid.uuid4()),
                    detail={"i": i},
                )
            )


async def _other_org() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        if await db.get(Organization, OTHER_ORG) is None:
            db.add(Organization(id=OTHER_ORG, name="org-b"))
    return OTHER_ORG


def _client_in(org_id: uuid.UUID) -> AsyncClient:
    from openrmm.api.app import create_app
    from openrmm.api.deps import current_user
    from openrmm.models.user import Role, User

    app = create_app()
    app.dependency_overrides[current_user] = lambda: User(
        id=uuid.uuid4(),
        email=f"{org_id}@example.com",
        password_hash="x",
        role=Role.admin,
        org_id=org_id,
    )
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://test")


async def _call(client, **params):
    """Through the real app: query parsing and validation are most of a route."""
    r = await client.get("/api/v1/audit", params={k: v for k, v in params.items() if v is not None})
    r.raise_for_status()
    return r.json()


async def test_newest_first(client):
    """An incident is read backwards from now."""
    await _entries(5)
    page = await _call(client, limit=5)
    times = [e["at"] for e in page["entries"]]
    assert times == sorted(times, reverse=True)


async def test_paging_uses_a_cursor_not_an_offset(client):
    """The table is append-only and being written to WHILE you page through
    it. An offset silently repeats and skips rows under concurrent writes;
    a keyset cursor cannot."""
    await _entries(10)

    first = await _call(client, limit=4)
    assert first["has_more"] is True
    assert first["next_before"] is not None

    second = await _call(client, limit=4, before=first["next_before"])
    assert second["entries"]

    ids = {e["id"] for e in first["entries"]} & {e["id"] for e in second["entries"]}
    assert ids == set(), "the same entry appeared on two pages"


async def test_the_last_page_says_so(client):
    await _entries(3)
    page = await _call(client, limit=50)
    assert page["has_more"] is False
    assert page["next_before"] is None


async def test_a_naive_cursor_is_refused(client):
    """Accepting it would page by the server's local clock without saying so,
    which silently returns the wrong window."""
    await _entries(2)
    r = await client.get("/api/v1/audit", params={"limit": 2, "before": "2026-01-01T00:00:00"})
    assert r.status_code == 422
    assert "timezone" in r.text


async def test_filters_narrow(client):
    await _entries(3, action="device.retired", actor="alice@example.com")
    await _entries(2, action="script.queued", actor="bob@example.com")

    assert len((await _call(client, action="device.retired", limit=50))["entries"]) == 3
    assert len((await _call(client, actor="bob@example.com", limit=50))["entries"]) == 2
    assert len((await _call(client, actor_type=ActorType.agent, limit=50))["entries"]) == 0


async def test_the_hours_window_excludes_older_entries(client):
    old = datetime.now(UTC) - timedelta(days=10)
    async with get_sessionmaker()() as db, db.begin():
        db.add(AuditLog(at=old, actor_type=ActorType.system, action="ancient.thing"))
    await _entries(2)

    recent = await _call(client, hours=1, limit=50)
    assert all(e["action"] != "ancient.thing" for e in recent["entries"])


async def test_a_device_history_includes_what_the_agent_itself_reported(client):
    """A device is named as the TARGET when an operator acts on it and as the
    ACTOR when its own agent reports. Reading only one of those halves the
    history of the machine you are investigating.

    The device is a real row now, because the endpoint authorizes on it: the
    audit trail names who ran what on which host, so serving it for an
    arbitrary UUID would hand another organization's operator identities to
    anyone who guessed one.
    """
    device_id = uuid7()
    async with get_sessionmaker()() as db, db.begin():
        db.add(Device(id=device_id, hostname="audited", os_family=OsFamily.linux, tags=[]))
    await _device_history(device_id)

    r = await client.get(f"/api/v1/audit/device/{device_id}")
    r.raise_for_status()
    assert {e["action"] for e in r.json()} == {"device.retired", "script.executed"}


async def _device_history(device_id: uuid.UUID, org_id: uuid.UUID = ORG) -> None:
    """The two shapes a device appears in: as a target, and as its own actor."""
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            AuditLog(
                org_id=org_id,
                actor_type=ActorType.user,
                actor_id="admin@example.com",
                action="device.retired",
                target_type="device",
                target_id=str(device_id),
            )
        )
        db.add(
            AuditLog(
                org_id=org_id,
                actor_type=ActorType.agent,
                actor_id=str(device_id),
                action="script.executed",
            )
        )


async def test_a_deleted_devices_history_outlives_it(client):
    """The whole reason the audit row is written BEFORE the device is deleted.

    Authorizing on the device row threw that away: the history of a machine
    somebody deleted, which is exactly the machine an incident is about, was
    unreachable the moment it mattered. The row carries its own org so the
    boundary does not depend on a parent that is gone.
    """
    device_id = uuid7()
    await _device_history(device_id)

    r = await client.get(f"/api/v1/audit/device/{device_id}")
    r.raise_for_status()
    assert {e["action"] for e in r.json()} == {"device.retired", "script.executed"}


async def test_the_real_deletion_path_leaves_a_readable_entry(client):
    """End to end, through the service that writes it, not a hand-made row.

    delete_device reads the organization off the device while it still exists;
    if it stopped doing that the entry would be filed under no organization at
    all and would be invisible to every reader, which looks exactly like never
    having been written.
    """
    from openrmm.models.device import DeviceStatus
    from openrmm.services.devices import delete_device

    device_id = uuid7()
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            Device(
                id=device_id,
                hostname="decommissioned",
                os_family=OsFamily.linux,
                status=DeviceStatus.retired,
                tags=[],
            )
        )
    async with get_sessionmaker()() as db, db.begin():
        await delete_device(db, device_id, actor="admin@example.com")

    r = await client.get(f"/api/v1/audit/device/{device_id}")
    r.raise_for_status()
    entry = r.json()[0]
    assert entry["action"] == "device.deleted"
    assert entry["detail"]["hostname"] == "decommissioned"


async def test_a_deleted_devices_history_stays_inside_its_organization():
    """And only to them. The trail names who ran what on which host, so
    serving it for any UUID hands another tenant's operator identities to
    anyone who guesses one. There is no device row left to check, so this is
    the only thing standing between the two."""
    await _other_org()
    device_id = uuid7()
    await _device_history(device_id, org_id=ORG)

    async with _client_in(OTHER_ORG) as c:
        r = await c.get(f"/api/v1/audit/device/{device_id}")
    assert r.status_code == 404, "another organization read a deleted device's history"

    # Indistinguishable from a device that never existed: the same 404, so the
    # response does not confirm that somebody else's machine was ever here.
    async with _client_in(OTHER_ORG) as c:
        assert (await c.get(f"/api/v1/audit/device/{uuid7()}")).status_code == 404


async def test_the_audit_list_shows_only_your_own_organization():
    """The device route is one URL; ?target_id= is another. Scoping one and
    not the other leaves the trail readable by whoever asks the second way."""
    await _other_org()
    device_id = uuid7()
    await _entries(3, action="mine.happened", target=str(device_id), org_id=ORG)
    await _entries(3, action="theirs.happened", target=str(device_id), org_id=OTHER_ORG)

    async with _client_in(ORG) as c:
        page = (await c.get("/api/v1/audit", params={"limit": 50})).json()
        actions = (await c.get("/api/v1/audit/actions")).json()

    assert {e["action"] for e in page["entries"]} == {"mine.happened"}
    assert "theirs.happened" not in actions, "the action filter enumerated another tenant's actions"


def test_the_page_size_is_capped():
    """The only table here that grows without bound. An unbounded query is a
    way to take the API down by accident."""
    assert MAX_LIMIT <= 200


async def test_actions_come_from_the_data(client):
    """Typing an action by hand means a typo returns an empty page that looks
    exactly like 'nothing happened'."""
    await _entries(2, action="device.retired")
    await _entries(1, action="script.queued")

    r = await client.get("/api/v1/audit/actions")
    r.raise_for_status()
    actions = r.json()
    assert "device.retired" in actions
    assert "script.queued" in actions
    assert actions == sorted(actions)
