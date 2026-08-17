"""Authorization, as opposed to authentication.

The existing suite proves every route requires *a* user. It has never proved
that a route requires the *right* user, because the client fixture hardcodes an
admin. These cover the two places that gap was actually costing something.

Both were found by a review, not by a test, and both are the same shape: a route
that reads back something the route which *creates* it correctly gates.
"""

import uuid

import pytest

from openrmm.db.engine import get_sessionmaker
from openrmm.models.alert import NotificationChannel
from openrmm.models.device import Device, OsFamily
from openrmm.models.script import ShellSession
from openrmm.models.user import Role
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

SECRET = "hmac-signing-secret-do-not-leak"
TOKEN = "gotify-app-token-do-not-leak"


async def _channel() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        ch = NotificationChannel(
            name="ops-webhook",
            kind="webhook",
            config={"url": "https://hooks.example.com/x", "secret": SECRET, "token": TOKEN},
            enabled=True,
        )
        db.add(ch)
        await db.flush()
        return ch.id


async def _session_with_recording() -> tuple[uuid.UUID, uuid.UUID]:
    async with get_sessionmaker()() as db, db.begin():
        device = Device(id=uuid7(), hostname="shell-host", os_family=OsFamily.linux, tags=[])
        db.add(device)
        await db.flush()
        s = ShellSession(
            id=uuid.uuid4(),
            device_id=device.id,
            recording_path=f"{uuid.uuid4()}.cast",
            bytes_in=10,
            bytes_out=200,
            close_reason="exit",
        )
        db.add(s)
        await db.flush()
        return device.id, s.id


# --- channel credentials -----------------------------------------------------


@pytest.mark.parametrize("role", [Role.viewer, Role.operator, Role.admin])
async def test_listing_channels_never_returns_a_credential(client_as, role):
    """No role, not even admin, has a reason to receive these over the wire.

    The browser cannot echo a credential back into an edit form (update_channel
    treats an absent key as "unchanged" precisely because of that), so nothing
    in the product needs the plaintext. Redaction on every path is therefore
    free, which is what makes role-gating the wrong fix here.
    """
    await _channel()
    async with client_as(role) as c:
        r = await c.get("/api/v1/alerts/channels")
        assert r.status_code == 200
        body = r.text
    assert SECRET not in body, f"{role.value} received the webhook signing secret"
    assert TOKEN not in body, f"{role.value} received the gotify token"

    # Redaction must also stop claiming no credential is set. The leaking
    # version reported secrets_set: [] while shipping both, so the response
    # actively contradicted itself.
    channel = r.json()[0]
    assert set(channel["secrets_set"]) == {"secret", "token"}
    assert channel["config"]["url"] == "https://hooks.example.com/x"


async def test_creating_a_channel_does_not_echo_the_credential_back(client_as):
    async with client_as(Role.admin) as c:
        r = await c.post(
            "/api/v1/alerts/channels",
            json={
                "name": "new-webhook",
                "kind": "webhook",
                "config": {"url": "https://x.example", "secret": SECRET},
                "enabled": True,
            },
        )
    assert r.status_code == 201
    assert SECRET not in r.text
    assert r.json()["secrets_set"] == ["secret"]


# --- shell session recordings ------------------------------------------------


async def test_a_viewer_cannot_list_root_shell_sessions(client_as):
    await _session_with_recording()
    async with client_as(Role.viewer) as c:
        r = await c.get("/api/v1/devices/sessions/recent")
    assert r.status_code == 403, "a viewer enumerated root shell sessions"


async def test_a_viewer_cannot_download_a_root_shell_recording(client_as):
    _, session_id = await _session_with_recording()
    async with client_as(Role.viewer) as c:
        r = await c.get(f"/api/v1/devices/sessions/{session_id}/recording")
    # 403 before the file is even resolved: a recording is a verbatim
    # transcript of a root session, including anything pasted into it.
    assert r.status_code == 403, "a viewer downloaded a root shell recording"


@pytest.mark.parametrize("role", [Role.operator, Role.admin])
async def test_operators_and_admins_may_still_read_sessions(client_as, role):
    """The gate must not lock out the people whose job this is.

    The WebSocket that opens a shell allows admin and operator, so the read-back
    has to match it, or an operator can hold a session they cannot review.
    """
    await _session_with_recording()
    async with client_as(role) as c:
        r = await c.get("/api/v1/devices/sessions/recent")
    assert r.status_code == 200
    assert len(r.json()) >= 1


async def test_the_schema_itself_refuses_to_carry_a_credential():
    """The unsafe object is unconstructable, not merely unconstructed.

    Relying on callers to choose `redacted()` is what failed: three routes, one
    of them safe. This asserts the other two cannot be reintroduced by someone
    reaching for the obvious constructor.
    """
    from openrmm.schemas.alert import ChannelOut

    leaked = ChannelOut(
        id=uuid.uuid4(),
        name="direct",
        kind="webhook",
        config={"url": "https://x.example", "secret": SECRET, "token": TOKEN},
        enabled=True,
    )
    assert "secret" not in leaked.config
    assert "token" not in leaked.config
    assert leaked.config["url"] == "https://x.example"
    assert set(leaked.secrets_set) == {"secret", "token"}
    assert SECRET not in leaked.model_dump_json()
