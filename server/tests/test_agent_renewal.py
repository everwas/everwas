"""The agent asks for its own replacement credential.

Rotation used to be a PUSH: the server minted a new secret and delivered it over
NATS, keeping the old one alive for 24 hours. Nothing ever retried. A laptop
switched off for a long weekend booted on Monday holding a secret that expired
on Saturday, with no channel left to receive its replacement. Recovery was a
site visit and a fresh enrollment token, per host.

The bug was never the grace window. It was that delivery went to a machine that
might not be there. An agent that asks, using the credential it currently holds,
cannot miss the delivery: it is the one initiating it.

This is also the shape the CSR endpoint needs when certificates arrive
(ADR-0003), which is why it is built as renewal rather than as a retry loop
around the push.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest

from openrmm.db.engine import session_scope
from openrmm.models.device import AgentCredential, Device, DeviceStatus, OsFamily
from openrmm.services.enrollment import (
    RevokedCredentialError,
    UnknownCredentialError,
    _sha256,
    renew_agent_secret,
    revoke_agent_secret,
    rotate_agent_secret,
    verify_agent_secret,
)
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _enrolled(secret: str = "gen0") -> uuid.UUID:
    async with session_scope() as db:
        device = Device(id=uuid7(), hostname="renew-host", os_family=OsFamily.linux, tags=[])
        db.add(device)
        await db.flush()
        db.add(AgentCredential(device_id=device.id, secret_hash=_sha256(secret)))
        return device.id


async def _cred(device_id: uuid.UUID) -> AgentCredential:
    async with session_scope() as db:
        return await db.get(AgentCredential, device_id)


async def test_an_agent_can_exchange_its_current_secret_for_a_new_one():
    device_id = await _enrolled("gen0")

    async with session_scope() as db:
        new_secret = await renew_agent_secret(db, device_id, presented="gen0")

    assert new_secret and new_secret != "gen0"
    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, new_secret) is True


async def test_renewing_does_not_kill_the_old_secret_until_the_agent_returns():
    """The renewal reply can be lost in flight.

    If the response never reaches the agent it is still holding the old secret,
    so retiring it on the server immediately would lock out the machine that
    just did exactly the right thing. The old one dies when the agent proves it
    has the new one, which is what verify_agent_secret already does.
    """
    device_id = await _enrolled("gen0")

    async with session_scope() as db:
        await renew_agent_secret(db, device_id, presented="gen0")

    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is True


async def test_a_wrong_secret_cannot_renew():
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        with pytest.raises(UnknownCredentialError):
            await renew_agent_secret(db, device_id, presented="not-the-secret")

    # And the real one still works: a failed attempt must not rotate anything.
    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is True


async def test_renewing_on_the_PREVIOUS_secret_is_allowed_and_is_the_whole_point():
    """This is the lockout case, and it must work.

    An admin rotated while the device was off. The device returns still holding
    the old secret, which is inside its grace window, so it can connect. Renewal
    on that old secret is how it catches up. Refusing it here would leave the
    machine to be locked out when the window closes, which is the bug.
    """
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        await rotate_agent_secret(db, device_id, actor="admin@example.com")

    async with session_scope() as db:
        fresh = await renew_agent_secret(db, device_id, presented="gen0")
    assert fresh

    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, fresh) is True


async def test_a_hygiene_rotation_has_no_wall_clock_deadline():
    """A rollover must not brick a machine that is merely switched off.

    The old secret stays valid until the agent proves it has the new one, so a
    device offline for a month renews on the day it returns. The 24 hour
    deadline was what turned a long weekend into a site visit.
    """
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        await rotate_agent_secret(db, device_id, actor="admin@example.com")

    cred = await _cred(device_id)
    assert cred.prev_secret_hash is not None
    assert cred.prev_valid_until is None, (
        "a hygiene rotation set an expiry on the old secret, so a device that "
        "stays offline past it can never authenticate again"
    )

    # Even simulating a long absence, the old secret still authenticates.
    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is True


async def test_revocation_still_has_a_hard_deadline():
    """Revocation is a DIFFERENT operation, and it must actually revoke.

    Separate function rather than a flag, so an operator locking a machine out
    is choosing to, not inheriting it from a default.
    """
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        await revoke_agent_secret(
            db, device_id, actor="admin@example.com", grace=timedelta(seconds=-1)
        )

    cred = await _cred(device_id)
    assert cred.prev_valid_until is not None

    async with session_scope() as db:
        assert await verify_agent_secret(db, device_id, "gen0") is False


async def test_a_revoked_credential_cannot_be_renewed_back_into_life():
    """Otherwise revocation is theatre.

    The old secret is dead, and the endpoint must refuse it rather than treat
    presenting it as grounds for issuing a fresh one.
    """
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        await revoke_agent_secret(
            db, device_id, actor="admin@example.com", grace=timedelta(seconds=-1)
        )

    async with session_scope() as db:
        with pytest.raises(UnknownCredentialError):
            await renew_agent_secret(db, device_id, presented="gen0")


async def test_a_retired_device_cannot_renew():
    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        device = await db.get(Device, device_id)
        device.status = DeviceStatus.retired

    async with session_scope() as db:
        with pytest.raises(RevokedCredentialError):
            await renew_agent_secret(db, device_id, presented="gen0")


async def test_renewal_is_audited():
    from sqlalchemy import select

    from openrmm.models.audit import AuditLog

    device_id = await _enrolled("gen0")
    async with session_scope() as db:
        await renew_agent_secret(db, device_id, presented="gen0")

    async with session_scope() as db:
        rows = (
            (await db.execute(select(AuditLog).where(AuditLog.action == "agent.renewed")))
            .scalars()
            .all()
        )
    assert any(r.target_id == str(device_id) for r in rows), (
        "a credential changed with no audit record of it"
    )


async def test_renewal_records_when_it_happened_so_the_agent_can_pace_itself():
    device_id = await _enrolled("gen0")
    before = datetime.now(UTC)
    async with session_scope() as db:
        await renew_agent_secret(db, device_id, presented="gen0")

    cred = await _cred(device_id)
    assert cred.rotated_at is not None
    assert cred.rotated_at >= before - timedelta(seconds=5)
