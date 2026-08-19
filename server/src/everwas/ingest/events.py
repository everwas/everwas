"""Agent audit events -> audit_log."""

import json
import uuid

import structlog
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.audit import ActorType, AuditLog
from everwas.services.audit import device_org

log = structlog.get_logger()


def parse_agent_event(subject: str, payload: bytes) -> tuple[uuid.UUID, dict] | None:
    parts = subject.split(".")
    if len(parts) != 3 or parts[2] != "events":
        return None
    try:
        device_id = uuid.UUID(parts[1])
        envelope = json.loads(payload)
    except (ValueError, json.JSONDecodeError):
        return None
    if envelope.get("agent_id") != str(device_id):
        return None
    return device_id, envelope.get("data") or {}


async def record_agent_event(db: AsyncSession, device_id: uuid.UUID, data: dict) -> None:
    event = data.get("event")
    if not event:
        return
    db.add(
        AuditLog(
            # Looked up: an agent event arrives off the wire with no caller to
            # inherit an organization from. An unknown device yields None, and
            # a row nobody can read, which is the right answer for an event
            # from a device that is not ours.
            org_id=await device_org(db, device_id),
            actor_type=ActorType.agent,
            actor_id=str(device_id),
            action=str(event)[:120],
            target_type="device",
            target_id=str(device_id),
            detail=data.get("detail") or {},
        )
    )
