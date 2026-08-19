"""Agent revocation and credential rotation.

Until now an agent secret was good for ever: nothing set status=retired,
nothing deleted a credential, and the issued NATS JWT had no expiry. A leaked
secret was permanent and there was no way to take a machine out of the fleet.

These tests pin the three properties that make revocation real, and the one
that makes rotation survivable.
"""

import time
import uuid
from datetime import UTC, datetime, timedelta

import pytest

from everwas.db.engine import get_sessionmaker, session_scope
from everwas.models.device import AgentCredential, Device, DeviceStatus, OsFamily
from everwas.natsio.auth_callout import _user_jwt
from everwas.natsio.jwt import decode_jwt_payload
from everwas.services.enrollment import (
    RotationInFlightError,
    retire_device,
    revoke_agent_secret,
    rotate_agent_secret,
    rotation_in_flight,
    verify_agent_secret,
)
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

# A throwaway account seed. Never used outside these tests.
TEST_SEED = "SAANDLKMXL6CUS3CP52WIXBEDN6YJ545GDKC65U5JZPPV6WH6ESWUA6YAI"


async def _enrolled(secret: str = "s3cret") -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        device = Device(id=uuid7(), hostname="revoke-test", os_family=OsFamily.linux)
        db.add(device)
        await db.flush()
        import hashlib

        db.add(
            AgentCredential(
                device_id=device.id,
                secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
            )
        )
    return device.id


async def test_retired_agent_cannot_authenticate():
    """The whole point. A retired machine must not get back in."""
    device_id = await _enrolled()

    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "s3cret") is True

    async with get_sessionmaker()() as db, db.begin():
        device = await retire_device(db, device_id, actor="admin@example.com")
        assert device is not None
        assert device.status is DeviceStatus.retired

    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "s3cret") is False


async def test_retire_deletes_the_credential():
    """Status alone is a check every future caller has to remember. Deleting
    the row means a caller that forgets still cannot verify anything."""
    device_id = await _enrolled()
    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        cred = await db.get(AgentCredential, device_id)
        assert cred is None


async def test_retire_is_audited():
    device_id = await _enrolled()
    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        from sqlalchemy import select

        from everwas.models.audit import AuditLog

        rows = (
            (await db.execute(select(AuditLog).where(AuditLog.action == "device.retired")))
            .scalars()
            .all()
        )
        assert len(rows) == 1
        assert rows[0].target_id == str(device_id)
        assert rows[0].actor_id == "admin@example.com"


async def test_retiring_an_unknown_device_is_not_an_error():
    async with get_sessionmaker()() as db, db.begin():
        assert await retire_device(db, uuid7(), actor="admin@example.com") is None


async def test_rotation_keeps_the_old_secret_alive():
    """The failure this prevents: the server rotates, the agent stores the new
    secret, the acknowledgement is lost. With one valid secret at a time the
    two disagree for ever and the machine needs a site visit. Both work during
    the window, so whichever one the agent ends up holding gets it back in."""
    device_id = await _enrolled("old-secret")

    async with get_sessionmaker()() as db, db.begin():
        new_secret = await rotate_agent_secret(db, device_id, actor="admin@example.com")
    assert new_secret is not None
    assert new_secret != "old-secret"

    # The old one FIRST: an agent that never got the message reconnects on it,
    # and that must work. Checking the new one first would clear the window
    # (see test_the_agent_connecting_confirms_the_rotation), which is right
    # but is not what this test is about.
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "old-secret") is True
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, new_secret) is True


async def test_the_old_secret_dies_when_the_window_closes():
    """Otherwise revocation is not revocation, it is a second permanent key.

    Now asserted against revoke_agent_secret rather than rotate_agent_secret.
    The two were one function with a grace argument, which meant the ordinary
    rollover inherited a wall-clock deadline it did not need: a device switched
    off past it could never authenticate again, and recovery was a site visit.
    Rollover now keeps the old secret until the agent proves it has the new one;
    only revocation kills on a clock, because only revocation is trying to.
    """
    device_id = await _enrolled("old-secret")

    async with get_sessionmaker()() as db, db.begin():
        new_secret = await revoke_agent_secret(
            db, device_id, actor="admin@example.com", grace=timedelta(seconds=-1)
        )

    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, new_secret) is True
        assert await verify_agent_secret(db, device_id, "old-secret") is False


async def test_rotation_does_not_chain_grandparent_secrets():
    """Two rotations must not leave three valid secrets. Only the immediately
    previous one survives, or a secret leaked long ago stays usable as long as
    rotations keep happening."""
    device_id = await _enrolled("gen0")

    async with get_sessionmaker()() as db, db.begin():
        gen1 = await rotate_agent_secret(db, device_id, actor="admin@example.com")
    # force, because the second rotation is exactly what the in-flight guard
    # refuses. This test is about what the credential table keeps, not about
    # whether the operator should be allowed to get here.
    async with get_sessionmaker()() as db, db.begin():
        gen2 = await rotate_agent_secret(db, device_id, actor="admin@example.com", force=True)

    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is False
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, gen1) is True
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, gen2) is True


async def test_rotating_a_retired_device_issues_nothing():
    device_id = await _enrolled()
    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db, db.begin():
        assert await rotate_agent_secret(db, device_id, actor="admin@example.com") is None


def test_issued_jwt_expires():
    """Auth-callout runs at CONNECT only. Without an expiry the session a
    machine already holds outlives every revocation decision behind it, so
    retiring a connected device changes nothing until someone reboots it."""
    agent_id = str(uuid.uuid4())
    issued = _user_jwt("ISSUER", TEST_SEED, "UUSER", agent_id, ttl_s=3600)

    claims = decode_jwt_payload(issued.encode())
    assert "exp" in claims, "the JWT never expires, so revocation never reaches a live agent"

    now = int(time.time())
    assert claims["exp"] > now
    assert claims["exp"] - claims["iat"] == 3600


def test_jwt_ttl_is_configurable():
    agent_id = str(uuid.uuid4())
    claims = decode_jwt_payload(_user_jwt("ISSUER", TEST_SEED, "UUSER", agent_id, 60).encode())
    assert claims["exp"] - claims["iat"] == 60


async def test_a_revocation_window_is_set_in_the_future():
    """Guards the sign of the arithmetic: a window computed backwards would
    invalidate the old secret instantly, locking out a machine that has not
    collected its replacement yet."""
    device_id = await _enrolled("old-secret")
    before = datetime.now(UTC)

    async with get_sessionmaker()() as db, db.begin():
        await revoke_agent_secret(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        cred = await db.get(AgentCredential, device_id)
        assert cred.prev_valid_until is not None
        assert cred.prev_valid_until > before


async def test_an_ordinary_rollover_sets_no_deadline_at_all():
    """The inverse, and the reason the split exists.

    A rollover that expires on a clock is a delivery deadline dressed as a
    security control: nothing is safer for it, because the old secret's real
    death is the agent proving it no longer needs it.
    """
    device_id = await _enrolled("old-secret")
    async with get_sessionmaker()() as db, db.begin():
        await rotate_agent_secret(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        cred = await db.get(AgentCredential, device_id)
        assert cred.prev_secret_hash is not None
        assert cred.prev_valid_until is None


async def test_a_second_rotation_is_refused_while_one_is_unconfirmed():
    """The lockout this guard exists for, reproduced.

    Rotation reports `delivered: false` when the agent did not answer, and the
    obvious reaction is to press it again. Only one generation of history is
    kept, so the second rotation discards the secret the agent is actually
    holding: it can never authenticate again and needs a site visit. Caught
    for real against a live VM agent on 2026-08-16.
    """
    device_id = await _enrolled("agent-holds-this")

    async with get_sessionmaker()() as db, db.begin():
        await rotate_agent_secret(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(RotationInFlightError):
            await rotate_agent_secret(db, device_id, actor="admin@example.com")

    # And the agent's secret still works, which is the point.
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "agent-holds-this") is True


async def test_force_overrides_the_guard():
    """For an operator who knows the machine is gone and wants the old secret
    dead now rather than in 24 hours."""
    device_id = await _enrolled("agent-holds-this")

    async with get_sessionmaker()() as db, db.begin():
        await rotate_agent_secret(db, device_id, actor="admin@example.com")
    async with get_sessionmaker()() as db, db.begin():
        forced = await rotate_agent_secret(db, device_id, actor="admin@example.com", force=True)

    assert forced is not None
    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "agent-holds-this") is False


async def test_the_agent_connecting_confirms_the_rotation():
    """A successful connect on the new secret is the only confirmation that
    cannot be lost or faked, so it is what clears the window. Without this,
    rotation stays 'in flight' for 24 hours after it demonstrably completed
    and a legitimate second rotation is refused for no reason."""
    device_id = await _enrolled("gen0")

    async with get_sessionmaker()() as db, db.begin():
        gen1 = await rotate_agent_secret(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        assert await rotation_in_flight(db, device_id) is True

    # The agent reconnects with the new secret.
    #
    # Through session_scope(), which is what the NATS auth callout uses. This
    # used to be a bare session with no transaction, and verify_agent_secret
    # committed its own clearing UPDATE, so the test passed against a session
    # shape production never uses. The commit now belongs to the caller, which
    # is why this has to open a transaction like the real caller does.
    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, gen1) is True

    async with get_sessionmaker()() as db:
        assert await rotation_in_flight(db, device_id) is False
        # ...and the old one is dead immediately, not in 24 hours.
        assert await verify_agent_secret(db, device_id, "gen0") is False

    # A further rotation is now allowed.
    async with get_sessionmaker()() as db, db.begin():
        assert await rotate_agent_secret(db, device_id, actor="admin@example.com") is not None


async def test_connecting_on_the_old_secret_does_not_confirm():
    """The agent is still on the previous secret, so the window must stay open
    or the next reconnect after the grace expires locks it out."""
    device_id = await _enrolled("gen0")

    async with get_sessionmaker()() as db, db.begin():
        await rotate_agent_secret(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is True

    async with get_sessionmaker()() as db:
        assert await rotation_in_flight(db, device_id) is True
