"""Reading the audit log.

It has been written to since M0 with no reader at all. For a tool whose
ordinary operations are "run this as root on 400 machines" and "revoke that
agent", a trail nobody can read is a trail that does not exist.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest

from openrmm.api.v1.audit import MAX_LIMIT
from openrmm.db.engine import get_sessionmaker
from openrmm.models.audit import ActorType, AuditLog

pytestmark = pytest.mark.usefixtures("pg_database")


async def _entries(n: int, *, action="device.retired", actor="admin@example.com", target=None):
    base = datetime.now(UTC)
    async with get_sessionmaker()() as db, db.begin():
        for i in range(n):
            db.add(
                AuditLog(
                    at=base - timedelta(minutes=i),
                    actor_type=ActorType.user,
                    actor_id=actor,
                    action=action,
                    target_type="device",
                    target_id=target or str(uuid.uuid4()),
                    detail={"i": i},
                )
            )


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
    history of the machine you are investigating."""
    device_id = uuid.uuid4()
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            AuditLog(
                actor_type=ActorType.user,
                actor_id="admin@example.com",
                action="device.retired",
                target_type="device",
                target_id=str(device_id),
            )
        )
        db.add(
            AuditLog(
                actor_type=ActorType.agent,
                actor_id=str(device_id),
                action="script.executed",
            )
        )

    r = await client.get(f"/api/v1/audit/device/{device_id}")
    r.raise_for_status()
    assert {e["action"] for e in r.json()} == {"device.retired", "script.executed"}


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
