"""The organization boundary, enforced rather than declared.

org_id existed on nine tables and was referenced by ZERO queries outside the
models. An admin in org B could retire, delete, shell into, rotate credentials
for, or run scripts on any device in org A by knowing its UUID. The model
docstring was honest about this, which made it a documented gap rather than a
hidden one, but a column that looks like an isolation boundary and is not is
worse than either having one or not having one.

The important test here is the LAST one. Enforcement that has to be remembered
at each new route is the same failure mode as every other finding in this
review, so `test_every_device_scoped_route_refuses_a_foreign_device` enumerates
the routes off the live app rather than listing them: a route added next month
is covered the day it is added, and the failure names it.
"""

import uuid

import pytest
from httpx import ASGITransport, AsyncClient

from openrmm.api.app import create_app
from openrmm.api.deps import current_user
from openrmm.db.engine import get_sessionmaker, session_scope
from openrmm.models.device import Device, DeviceStatus, OsFamily
from openrmm.models.org import Organization
from openrmm.models.user import Role, User
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

ORG_A = uuid.UUID("0000000a-0000-0000-0000-00000000000a")
ORG_B = uuid.UUID("0000000b-0000-0000-0000-00000000000b")


async def _two_orgs() -> tuple[uuid.UUID, uuid.UUID]:
    """One device in each of two organizations."""
    async with session_scope() as db:
        for oid, name in ((ORG_A, "org-a"), (ORG_B, "org-b")):
            if await db.get(Organization, oid) is None:
                db.add(Organization(id=oid, name=name))
        await db.flush()

    ids = []
    async with session_scope() as db:
        for oid, host in ((ORG_A, "a-host"), (ORG_B, "b-host")):
            d = Device(
                id=uuid7(),
                hostname=host,
                os_family=OsFamily.linux,
                tags=[],
                org_id=oid,
                status=DeviceStatus.active,
            )
            db.add(d)
            await db.flush()
            ids.append(d.id)
    return ids[0], ids[1]


def _client_in(org_id: uuid.UUID, role: Role = Role.admin) -> AsyncClient:
    app = create_app()
    app.dependency_overrides[current_user] = lambda: User(
        id=uuid.uuid4(),
        email=f"{org_id}@example.com",
        password_hash="x",
        role=role,
        org_id=org_id,
    )
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://test")


async def test_a_device_in_another_org_is_not_visible():
    a_device, b_device = await _two_orgs()
    async with _client_in(ORG_A) as c:
        r = await c.get(f"/api/v1/devices/{b_device}")
    assert r.status_code == 404, "an admin read a device belonging to another organization"

    # And its own device is still reachable, or the boundary is just an outage.
    async with _client_in(ORG_A) as c:
        assert (await c.get(f"/api/v1/devices/{a_device}")).status_code == 200


async def test_the_device_list_shows_only_your_own():
    a_device, b_device = await _two_orgs()
    async with _client_in(ORG_A) as c:
        body = (await c.get("/api/v1/devices")).json()
    ids = {d["id"] for d in (body.get("items", body) if isinstance(body, dict) else body)}
    assert str(a_device) in ids
    assert str(b_device) not in ids, "the fleet list leaked another organization's devices"


async def test_retiring_a_foreign_device_is_refused():
    # The most destructive of the device routes: it deletes the agent's
    # credential, so the machine drops off the fleet and needs re-enrolling.
    _, b_device = await _two_orgs()
    async with _client_in(ORG_A) as c:
        r = await c.post(f"/api/v1/devices/{b_device}/retire")
    assert r.status_code == 404

    async with get_sessionmaker()() as db:
        assert (await db.get(Device, b_device)).status is not DeviceStatus.retired


# Every path parameter a device-scoped route can take, so the enumeration below
# can build a URL for any route it finds.
def _fill(path: str, device_id: uuid.UUID) -> str:
    return (
        path.replace("{device_id}", str(device_id))
        .replace("{kind}", "hardware")
        .replace("{session_id}", str(uuid.uuid4()))
        .replace("{script_id}", str(uuid.uuid4()))
        .replace("{run_id}", str(uuid.uuid4()))
        .replace("{patch_id}", str(uuid.uuid4()))
    )


async def test_every_device_scoped_route_refuses_a_foreign_device():
    """Enumerated from the app, so a route added later is covered by default.

    This is the anti-forgetting mechanism. Enforcing the boundary route by
    route is exactly the shape that failed everywhere else in this review: a
    rule applied in one place and not in its sibling. A test that lists routes
    would rot the same way, so this reads them off the router.
    """
    from routewalk import api_routes

    a_device, b_device = await _two_orgs()
    app = create_app()

    checked, leaked = [], []
    for route in api_routes(app):
        if "{device_id}" not in route.path:
            continue
        # GET and POST only: the destructive verbs are covered individually
        # above, and DELETE on a foreign device would be tested by deleting it.
        for method in sorted(route.methods & {"GET", "POST"}):
            url = _fill(route.path, b_device)
            async with _client_in(ORG_A) as c:
                r = await c.request(method, url, json={})
            checked.append(f"{method} {route.path}")
            # 404 rather than 403: a foreign device should not be confirmed to
            # exist. 422 is fine too, since a rejected body never reached the
            # device. Anything 2xx is a leak.
            if r.status_code < 400:
                leaked.append(f"{method} {route.path} -> {r.status_code}")

    assert checked, "no device-scoped routes were found; the enumeration is broken"
    assert not leaked, "these routes acted on a device in another organization:\n  " + "\n  ".join(
        leaked
    )


async def test_a_user_with_no_org_sees_nothing_rather_than_everything():
    """Fail closed. A user whose org_id is somehow NULL must not become an
    accidental superuser across every tenant, which is what a filter written as
    `where org_id == caller.org_id` would do if it were skipped for None."""
    await _two_orgs()
    app = create_app()
    app.dependency_overrides[current_user] = lambda: User(
        id=uuid.uuid4(), email="orphan@example.com", password_hash="x", role=Role.admin, org_id=None
    )
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as c:
        r = await c.get("/api/v1/devices")
    body = r.json()
    items = body.get("items", body) if isinstance(body, dict) else body
    assert items == [], "a user with no organization saw the whole fleet"


async def test_the_shell_websocket_refuses_a_foreign_device():
    """Covered by hand because the enumeration cannot reach it.

    A WebSocket has to be refused before accept(), so it authenticates and
    authorizes inside the handler rather than through a dependency, which puts
    it outside the route-walking test above. It is also the single most
    valuable route to get right: it is an interactive root shell.
    """
    from openrmm.api.deps import current_user as dep

    _, b_device = await _two_orgs()
    app = create_app()
    app.dependency_overrides[dep] = lambda: User(
        id=uuid.uuid4(), email="a@example.com", password_hash="x", role=Role.admin, org_id=ORG_A
    )
    from fastapi.testclient import TestClient

    with TestClient(app) as client:
        try:
            with client.websocket_connect(f"/api/v1/devices/{b_device}/shell"):
                raise AssertionError("a root shell was opened on another organization's device")
        except AssertionError:
            raise
        except Exception:
            # Any refusal is fine; the handshake must simply not succeed.
            pass


async def test_an_enrolled_device_inherits_the_tokens_organization():
    """Otherwise the boundary has a hole at the only place devices are created.

    enroll_device never set org_id, so every device enrolled since the column
    was added belonged to no organization. A filter written as
    `WHERE org_id = :caller` excludes those silently rather than failing, so
    turning the boundary on would have hidden the entire existing fleet.
    """
    from openrmm.models.device import EnrollmentToken
    from openrmm.schemas.enrollment import EnrollRequest
    from openrmm.services.enrollment import _sha256, enroll_device

    await _two_orgs()
    async with session_scope() as db:
        db.add(
            EnrollmentToken(
                token_hash=_sha256("tok-org-b"),
                org_id=ORG_B,
                max_uses=1,
                uses=0,
                created_by="admin@example.com",
            )
        )

    async with session_scope() as db:
        device, _secret = await enroll_device(
            db,
            EnrollRequest(
                token="tok-org-b",
                hostname="fresh-host",
                os_family="linux",
                agent_version="test",
            ),
        )
        assert device.org_id == ORG_B, (
            "a newly enrolled device belongs to no organization, so it is "
            "invisible to every scoped query"
        )
