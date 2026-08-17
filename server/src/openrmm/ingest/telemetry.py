"""Telemetry ingest: partitioned history insert + hot-cache upsert."""

import json
import uuid
from datetime import datetime

import structlog
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.ingest.wiretime import WireTimeError, parse_wire_time
from openrmm.models.telemetry import (
    DeviceStatusLatest,
    telemetry_disks,
    telemetry_metrics,
    telemetry_network,
)

# Postgres has no unsigned integer type, and the agent reports counters as
# uint64. A value above this is unrepresentable in bigint and asyncpg raises
# rather than truncating, which would fail the whole telemetry sample over one
# bad NIC counter. Nothing real gets near 9.2 exabytes, so a value up here is a
# driver reporting garbage; drop the field and keep the rest of the sample.
BIGINT_MAX = 2**63 - 1

COUNTER_FIELDS = (
    "bytes_sent",
    "bytes_recv",
    "packets_sent",
    "packets_recv",
    "err_in",
    "err_out",
    "drop_in",
    "drop_out",
)


def _counter(value: object) -> int | None:
    """Coerce one wire counter, or None if it is not a usable bigint."""
    if not isinstance(value, int) or isinstance(value, bool):
        return None
    if value < 0 or value > BIGINT_MAX:
        return None
    return value


log = structlog.get_logger()


def parse_telemetry(subject: str, payload: bytes) -> tuple[uuid.UUID, datetime, dict] | None:
    parts = subject.split(".")
    if len(parts) != 3:
        return None
    try:
        agent_id = uuid.UUID(parts[1])
        envelope = json.loads(payload)
    except (ValueError, json.JSONDecodeError):
        return None
    if envelope.get("agent_id") != str(agent_id):
        return None
    try:
        ts = parse_wire_time(envelope.get("ts"))
    except WireTimeError as exc:
        # Dropped, not retried: a skewed clock does not fix itself, and this
        # value is the partition key.
        log.warning("rejecting telemetry timestamp", agent_id=str(agent_id), reason=str(exc))
        return None
    return agent_id, ts, envelope.get("data") or {}


async def apply_telemetry(db: AsyncSession, device_id: uuid.UUID, ts: datetime, data: dict) -> None:
    mem_used, mem_total = data.get("mem_used"), data.get("mem_total")
    mem_pct = (mem_used / mem_total * 100.0) if mem_used and mem_total else None

    await db.execute(
        pg_insert(telemetry_metrics)
        .values(
            device_id=device_id,
            ts=ts,
            cpu_pct=data.get("cpu_pct"),
            mem_used=mem_used,
            mem_total=mem_total,
            swap_pct=data.get("swap_pct"),
            load1=data.get("load1"),
            uptime_s=data.get("uptime_s"),
        )
        .on_conflict_do_nothing()  # JetStream redelivery safety
    )

    disks = [d for d in (data.get("disks") or []) if d.get("mount")]
    worst_disk_pct = None
    if disks:
        await db.execute(
            pg_insert(telemetry_disks)
            .values(
                [
                    {
                        "device_id": device_id,
                        "ts": ts,
                        "mount": d["mount"],
                        "used": d.get("used"),
                        "total": d.get("total"),
                        "fstype": d.get("fstype"),
                    }
                    for d in disks
                ]
            )
            .on_conflict_do_nothing()
        )
        pcts = [d["used"] / d["total"] * 100.0 for d in disks if d.get("used") and d.get("total")]
        worst_disk_pct = max(pcts) if pcts else None

    # Interfaces are keyed by name and the name is the primary key, so a
    # sample that repeats one (bonding oddities, a driver listing an alias
    # twice) would abort the insert on a duplicate key. Last one wins.
    nets: dict[str, dict] = {}
    for n in data.get("nets") or []:
        name = n.get("name")
        if not name:
            continue
        row = {"device_id": device_id, "ts": ts, "iface": str(name)}
        row.update({f: _counter(n.get(f)) for f in COUNTER_FIELDS})
        nets[str(name)] = row
    if nets:
        await db.execute(
            pg_insert(telemetry_network).values(list(nets.values())).on_conflict_do_nothing()
        )

    upsert = pg_insert(DeviceStatusLatest.__table__).values(
        device_id=device_id,
        ts=ts,
        cpu_pct=data.get("cpu_pct"),
        mem_pct=mem_pct,
        worst_disk_pct=worst_disk_pct,
    )
    await db.execute(
        upsert.on_conflict_do_update(
            index_elements=["device_id"],
            set_={
                "ts": upsert.excluded.ts,
                "cpu_pct": upsert.excluded.cpu_pct,
                "mem_pct": upsert.excluded.mem_pct,
                "worst_disk_pct": upsert.excluded.worst_disk_pct,
            },
            where=DeviceStatusLatest.__table__.c.ts < upsert.excluded.ts,
        )
    )
