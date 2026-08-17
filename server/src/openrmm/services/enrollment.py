import hashlib
import secrets
import uuid
from datetime import UTC, datetime, timedelta

from sqlalchemy import delete, select, update
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import AgentCredential, Device, DeviceStatus, EnrollmentToken
from openrmm.schemas.enrollment import EnrollRequest
from openrmm.util.ids import uuid7

TOKEN_PREFIX = "ore_"

# How long the superseded secret keeps working after a rotation. Long enough
# for an offline agent to come back and be told, short enough that a leaked
# secret is not useful for long.
ROTATION_GRACE = timedelta(hours=24)


def _sha256(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


class EnrollmentError(Exception):
    """Invalid, exhausted, or expired enrollment token."""


async def mint_enrollment_token(
    db: AsyncSession,
    *,
    site_id: uuid.UUID | None = None,
    max_uses: int = 1,
    ttl_hours: int | None = 24,
    created_by: str | None = None,
) -> tuple[EnrollmentToken, str]:
    token = TOKEN_PREFIX + secrets.token_urlsafe(24)
    row = EnrollmentToken(
        token_hash=_sha256(token),
        site_id=site_id,
        max_uses=max_uses,
        uses=0,
        expires_at=datetime.now(UTC) + timedelta(hours=ttl_hours) if ttl_hours else None,
        created_by=created_by,
    )
    db.add(row)
    await db.flush()
    return row, token


async def enroll_device(db: AsyncSession, req: EnrollRequest) -> tuple[Device, str]:
    row = await db.execute(
        select(EnrollmentToken)
        .where(EnrollmentToken.token_hash == _sha256(req.token))
        .with_for_update()
    )
    token = row.scalar_one_or_none()
    if token is None:
        raise EnrollmentError("unknown token")
    if token.uses >= token.max_uses:
        raise EnrollmentError("token exhausted")
    if token.expires_at is not None and token.expires_at < datetime.now(UTC):
        raise EnrollmentError("token expired")
    token.uses += 1

    device = Device(
        id=uuid7(),
        site_id=token.site_id,
        hostname=req.hostname,
        os_family=req.os_family,
        os_version=req.os_version,
        arch=req.arch,
        agent_version=req.agent_version,
    )
    db.add(device)
    # Flush so the device row exists before agent_credentials references it —
    # without an ORM relationship() the unit of work won't order these inserts.
    await db.flush()

    agent_secret = secrets.token_urlsafe(32)
    db.add(AgentCredential(device_id=device.id, secret_hash=_sha256(agent_secret)))

    db.add(
        AuditLog(
            actor_type=ActorType.agent,
            actor_id=str(device.id),
            action="device.enrolled",
            target_type="device",
            target_id=str(device.id),
            detail={"hostname": req.hostname, "os_family": req.os_family.value},
        )
    )
    await db.flush()
    return device, agent_secret


async def verify_agent_secret(db: AsyncSession, agent_id: uuid.UUID, secret: str) -> bool:
    """Check an agent's presented secret. Retired devices are always refused.

    The previous secret is honoured until `prev_valid_until` so an interrupted
    rotation cannot lock a machine out permanently. Outside that window it is
    dead, which is what makes rotation a revocation rather than a second key.
    """
    row = await db.execute(
        select(
            AgentCredential.secret_hash,
            AgentCredential.prev_secret_hash,
            AgentCredential.prev_valid_until,
            Device.status,
        )
        .join(Device, Device.id == AgentCredential.device_id)
        .where(AgentCredential.device_id == agent_id)
    )
    rec = row.first()
    if rec is None or rec.status is DeviceStatus.retired:
        return False

    presented = _sha256(secret)
    # compare_digest on both branches: an early return on the current secret
    # would make "you are on the old one" measurably slower than "you are on
    # the new one".
    current_ok = secrets.compare_digest(rec.secret_hash, presented)
    prev_ok = (
        rec.prev_secret_hash is not None
        and rec.prev_valid_until is not None
        and rec.prev_valid_until > datetime.now(UTC)
        and secrets.compare_digest(rec.prev_secret_hash, presented)
    )

    if current_ok and rec.prev_secret_hash is not None:
        # The agent is demonstrably on the new secret, which is the only proof
        # of that we ever get: the rotation reply can be lost, but a connection
        # cannot be faked. Retiring the old hash here is what lets
        # rotation_in_flight() be precise instead of "some time in the last 24
        # hours somebody pressed rotate".
        await db.execute(
            update(AgentCredential)
            .where(AgentCredential.device_id == agent_id)
            .values(prev_secret_hash=None, prev_valid_until=None)
        )
        # No commit here: the caller owns the transaction. session_scope() is
        # `sessionmaker() as s, s.begin()`, so it commits on clean exit.
        #
        # There used to be a db.commit() on this line. It did not raise, only
        # because it was the last statement before the return: SQLAlchemy
        # objects to work emitted AFTER a commit inside an explicit begin()
        # block, not to the commit itself. So this was one added line away from
        # raising inside the NATS auth callout, where the blanket except means
        # msg.respond is never called and the agent's connect dies on a timeout
        # rather than on a decision.

    return current_ok or prev_ok


async def rotation_in_flight(db: AsyncSession, device_id: uuid.UUID) -> bool:
    """Is a rotation still unconfirmed for this device?

    True means the agent has not yet been seen using the current secret, so it
    is presumably still on the previous one.
    """
    rec = (
        await db.execute(
            select(AgentCredential.prev_secret_hash, AgentCredential.prev_valid_until).where(
                AgentCredential.device_id == device_id
            )
        )
    ).first()
    return (
        rec is not None
        and rec.prev_secret_hash is not None
        and rec.prev_valid_until is not None
        and rec.prev_valid_until > datetime.now(UTC)
    )


class RotationInFlightError(Exception):
    """A rotation is already outstanding and has not been confirmed."""


async def retire_device(db: AsyncSession, device_id: uuid.UUID, actor: str) -> Device | None:
    """Revoke an agent for good. Returns None if the device does not exist.

    The credential row is deleted rather than left in place: status alone
    depends on every future caller remembering to check it, and one that
    forgets hands out a JWT to a machine that was retired months ago.

    This does NOT kick the live connection. NATS runs auth-callout at connect
    time only, so a connected agent keeps its session until the JWT expires
    (see nats_jwt_ttl_s) or the link drops. That bound is the revocation
    latency, and it is why the JWT has an expiry at all.
    """
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None:
        return None

    device.status = DeviceStatus.retired
    await db.execute(delete(AgentCredential).where(AgentCredential.device_id == device_id))
    db.add(
        AuditLog(
            actor_type=ActorType.user,
            actor_id=actor,
            action="device.retired",
            target_type="device",
            target_id=str(device_id),
            detail={"hostname": device.hostname},
        )
    )
    await db.flush()
    return device


async def rotate_agent_secret(
    db: AsyncSession,
    device_id: uuid.UUID,
    actor: str,
    *,
    grace: timedelta = ROTATION_GRACE,
    force: bool = False,
) -> str | None:
    """Issue a new agent secret, keeping the old one alive for `grace`.

    Returns the new secret, which is the only time it exists in plaintext, or
    None if the device has no credential row (never enrolled, or retired).

    The caller must hand the secret to the agent and commit. If that delivery
    fails, commit anyway: both secrets are valid right now, so the agent is
    reachable either way, and the alternative (rolling back) loses the record
    that a rotation was attempted.

    Raises RotationInFlightError if the agent has not been seen using the
    current secret yet. Only ONE generation of history is kept, so a second
    rotation over an unconfirmed one discards the secret the agent is actually
    holding and the machine can never authenticate again. That is a site visit
    per host, and the obvious thing to do when a rotation reports
    `delivered: false` is press it again, so this has to be refused rather
    than documented. `force` is the deliberate override for an operator who
    knows the agent is gone anyway.
    """
    if not force and await rotation_in_flight(db, device_id):
        raise RotationInFlightError(
            "a previous rotation has not been confirmed by the agent; rotating again "
            "would discard the secret it is still using"
        )

    cred = (
        await db.execute(select(AgentCredential).where(AgentCredential.device_id == device_id))
    ).scalar_one_or_none()
    if cred is None:
        return None

    new_secret = secrets.token_urlsafe(32)
    cred.prev_secret_hash = cred.secret_hash
    cred.prev_valid_until = datetime.now(UTC) + grace
    cred.secret_hash = _sha256(new_secret)
    cred.rotated_at = datetime.now(UTC)
    db.add(
        AuditLog(
            actor_type=ActorType.user,
            actor_id=actor,
            action="device.credentials_rotated",
            target_type="device",
            target_id=str(device_id),
            detail={"grace_s": int(grace.total_seconds())},
        )
    )
    await db.flush()
    return new_secret
