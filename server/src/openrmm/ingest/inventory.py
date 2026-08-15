"""Inventory ingest: routes snapshot kinds to the right store.

- hardware, software, patchstate -> bitemporal fact tables (sequenced amend)
- processes, services            -> device_snapshots (latest-only; too churny
                                    for belief history)
"""

import json
import uuid
from datetime import UTC, datetime

import structlog
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.bitemporal.store import record_facts
from openrmm.models.telemetry import DeviceSnapshot

log = structlog.get_logger()

FACT_KINDS = {"hardware", "software", "patchstate"}
SNAPSHOT_KINDS = {"processes", "services"}


def parse_inventory(subject: str, payload: bytes) -> tuple[uuid.UUID, str, datetime, dict] | None:
    parts = subject.split(".")
    if len(parts) != 4 or parts[3] not in FACT_KINDS | SNAPSHOT_KINDS:
        return None
    try:
        agent_id = uuid.UUID(parts[1])
        envelope = json.loads(payload)
        ts = datetime.fromisoformat(envelope["ts"].replace("Z", "+00:00"))
    except (ValueError, KeyError, json.JSONDecodeError):
        return None
    if envelope.get("agent_id") != str(agent_id):
        return None
    return agent_id, parts[3], ts.astimezone(UTC), envelope.get("data") or {}


def _facts_from(kind: str, data: dict) -> dict[str, dict]:
    """Flatten a snapshot payload into fact_key -> payload."""
    if kind == "software":
        return {
            f"pkg:{p['name']}": {"version": p.get("version", "")}
            for p in data.get("packages", [])
            if p.get("name")
        }
    if kind == "hardware":
        return {
            "cpu": {"model": data.get("cpu_model", ""), "cores": data.get("cpu_cores")},
            "memory": {"total": data.get("mem_total")},
            "os": {
                "family": data.get("os_family", ""),
                "version": data.get("os_version", ""),
                "kernel": data.get("kernel", ""),
            },
            "system": {
                "hostname": data.get("hostname", ""),
                "arch": data.get("arch", ""),
                "virtualization": data.get("virtualization", ""),
            },
        }
    if kind == "patchstate":
        return {
            f"patch:{p['id']}": {k: v for k, v in p.items() if k != "id"}
            for p in data.get("patches", [])
            if p.get("id")
        }
    raise ValueError(kind)


async def apply_inventory(
    db: AsyncSession, device_id: uuid.UUID, kind: str, observed_at: datetime, data: dict
) -> None:
    snapshot_hash = data.pop("snapshot_hash", "") if isinstance(data, dict) else ""

    if kind in FACT_KINDS:
        result = await record_facts(
            db, kind, device_id, _facts_from(kind, data), observed_at=observed_at
        )
        if result.wrote:
            log.info(
                "facts amended",
                device_id=str(device_id),
                kind=kind,
                added=result.added,
                changed=result.changed,
                removed=result.removed,
            )
        return

    upsert = pg_insert(DeviceSnapshot.__table__).values(
        device_id=device_id,
        kind=kind,
        payload=data,
        snapshot_hash=snapshot_hash,
        updated_at=datetime.now(UTC),
    )
    await db.execute(
        upsert.on_conflict_do_update(
            index_elements=["device_id", "kind"],
            set_={
                "payload": upsert.excluded.payload,
                "snapshot_hash": upsert.excluded.snapshot_hash,
                "updated_at": upsert.excluded.updated_at,
            },
            # skip identical snapshots (hash provided by the agent)
            where=(DeviceSnapshot.__table__.c.snapshot_hash != upsert.excluded.snapshot_hash),
        )
    )
