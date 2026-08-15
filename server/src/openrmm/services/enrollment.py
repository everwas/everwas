import hashlib
import secrets
import uuid
from datetime import UTC, datetime, timedelta

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import AgentCredential, Device, EnrollmentToken
from openrmm.schemas.enrollment import EnrollRequest
from openrmm.util.ids import uuid7

TOKEN_PREFIX = "ore_"


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
    row = await db.execute(
        select(AgentCredential.secret_hash, Device.status)
        .join(Device, Device.id == AgentCredential.device_id)
        .where(AgentCredential.device_id == agent_id)
    )
    rec = row.first()
    if rec is None or rec.status == "retired":
        return False
    return secrets.compare_digest(rec.secret_hash, _sha256(secret))
