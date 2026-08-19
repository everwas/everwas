"""Heartbeat ingest + offline sweep. Pure functions; wiring lives in dispatcher/main."""

import json
import uuid
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import select, update
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.device import Device, DeviceStatus

log = structlog.get_logger()


def parse_heartbeat(subject: str, payload: bytes) -> tuple[uuid.UUID, dict] | None:
    """`agents.{agent_id}.heartbeat` -> (agent_id, data). None if malformed."""
    parts = subject.split(".")
    if len(parts) != 3:
        return None
    try:
        agent_id = uuid.UUID(parts[1])
        envelope = json.loads(payload)
    except (ValueError, json.JSONDecodeError):
        return None
    if envelope.get("agent_id") != str(agent_id):
        log.warning("heartbeat agent_id mismatch", subject=subject)
        return None
    return agent_id, envelope.get("data") or {}


def _reported_certificate(data: dict) -> dict:
    """What the agent says it is holding, as columns to write.

    An agent that omits the field entirely is one too old to report, or one
    whose fleet does not use 802.1X. That is NOT the same as an agent reporting
    it holds nothing, which means the material is genuinely gone, so the absent
    case must leave the stored value alone rather than clearing it: overwriting
    a known serial with NULL every thirty seconds because half the fleet has
    not been upgraded yet would erase the very signal this exists to carry.
    """
    if "cert_serial" not in data:
        return {}

    serial = str(data.get("cert_serial") or "")[:64]
    not_after = None
    if raw := data.get("cert_not_after"):
        try:
            not_after = datetime.fromisoformat(str(raw))
        except ValueError:
            # A malformed timestamp from a broken agent build must not cost us
            # the serial, which is the part that matters.
            log.warning("heartbeat carried an unparseable certificate expiry", value=raw)

    return {
        "reported_cert_serial": serial or None,
        "reported_cert_not_after": not_after,
        "reported_cert_at": datetime.now(UTC),
    }


async def apply_heartbeat(db: AsyncSession, agent_id: uuid.UUID, data: dict) -> Device | None:
    """Mark the device active. Returns the device, or None if unknown."""
    values: dict = {
        "last_heartbeat_at": datetime.now(UTC),
        "status": DeviceStatus.active,
    }
    if data.get("version"):
        values["agent_version"] = str(data["version"])[:64]
    values.update(_reported_certificate(data))
    rows = await db.execute(
        update(Device)
        .where(Device.id == agent_id, Device.status != DeviceStatus.retired)
        .values(**values)
        .returning(Device)
    )
    return rows.scalar_one_or_none()


async def offline_devices(db: AsyncSession) -> list[Device]:
    """Every device currently offline.

    Heartbeat-missed must be evaluated as a LEVEL condition, not an edge: the
    sweep below only returns devices that just transitioned, so a device whose
    first alert attempt was suppressed (cooldown, a transient error) would
    never get another chance and would sit offline forever with an empty alert
    page. Re-evaluating every offline device each sweep is safe because the
    partial unique index makes a duplicate open a no-op.
    """
    rows = await db.execute(select(Device).where(Device.status == DeviceStatus.offline))
    return list(rows.scalars())


async def sweep_offline(db: AsyncSession, offline_after_s: int) -> list[Device]:
    """Flip active devices with stale heartbeats to offline.

    Returns the devices that just went offline so the caller can fire
    heartbeat-missed alerts for them.
    """
    cutoff = datetime.now(UTC) - timedelta(seconds=offline_after_s)
    rows = await db.execute(
        update(Device)
        .where(Device.status == DeviceStatus.active, Device.last_heartbeat_at < cutoff)
        .values(status=DeviceStatus.offline)
        .returning(Device)
    )
    return list(rows.scalars())
