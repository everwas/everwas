"""Heartbeat ingest + offline sweep. Pure functions; wiring lives in dispatcher/main."""

import json
import uuid
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import update
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.device import Device, DeviceStatus

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


async def apply_heartbeat(db: AsyncSession, agent_id: uuid.UUID, data: dict) -> bool:
    """Mark the device active. Returns False for unknown devices."""
    values: dict = {
        "last_heartbeat_at": datetime.now(UTC),
        "status": DeviceStatus.active,
    }
    if data.get("version"):
        values["agent_version"] = str(data["version"])[:64]
    result = await db.execute(
        update(Device)
        .where(Device.id == agent_id, Device.status != DeviceStatus.retired)
        .values(**values)
    )
    return result.rowcount > 0


async def sweep_offline(db: AsyncSession, offline_after_s: int) -> int:
    """Flip active devices with stale heartbeats to offline. Returns count flipped."""
    cutoff = datetime.now(UTC) - timedelta(seconds=offline_after_s)
    result = await db.execute(
        update(Device)
        .where(Device.status == DeviceStatus.active, Device.last_heartbeat_at < cutoff)
        .values(status=DeviceStatus.offline)
    )
    return result.rowcount
