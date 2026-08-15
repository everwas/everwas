import uuid
from datetime import UTC, datetime, timedelta
from typing import Literal

from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession
from openrmm.bitemporal.query import get_facts
from openrmm.models.device import Device
from openrmm.models.telemetry import DeviceSnapshot, DeviceStatusLatest, telemetry_metrics
from openrmm.schemas.device import DeviceDetailOut, DeviceOut, FactOut, TelemetryPoint

router = APIRouter()


async def _device_or_404(db, device_id: uuid.UUID) -> Device:
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown device")
    return device


@router.get("")
async def list_devices(db: DbSession, _user: CurrentUser) -> list[DeviceOut]:
    rows = await db.execute(select(Device).order_by(Device.hostname))
    return [DeviceOut.model_validate(d) for d in rows.scalars()]


@router.get("/{device_id}")
async def get_device(device_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> DeviceDetailOut:
    device = await _device_or_404(db, device_id)
    latest = (
        await db.execute(
            select(DeviceStatusLatest).where(DeviceStatusLatest.device_id == device_id)
        )
    ).scalar_one_or_none()
    out = DeviceDetailOut.model_validate(device)
    if latest is not None:
        out.cpu_pct = latest.cpu_pct
        out.mem_pct = latest.mem_pct
        out.worst_disk_pct = latest.worst_disk_pct
    return out


@router.get("/{device_id}/telemetry")
async def get_telemetry(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    hours: int = Query(default=24, ge=1, le=168),
) -> list[TelemetryPoint]:
    await _device_or_404(db, device_id)
    since = datetime.now(UTC) - timedelta(hours=hours)
    t = telemetry_metrics
    rows = await db.execute(
        select(t.c.ts, t.c.cpu_pct, t.c.mem_used, t.c.mem_total, t.c.load1)
        .where(t.c.device_id == device_id, t.c.ts >= since)
        .order_by(t.c.ts)
    )
    return [
        TelemetryPoint(
            ts=r.ts,
            cpu_pct=r.cpu_pct,
            mem_pct=(r.mem_used / r.mem_total * 100.0) if r.mem_used and r.mem_total else None,
            load1=r.load1,
        )
        for r in rows
    ]


@router.get("/{device_id}/facts")
async def get_device_facts(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    kind: Literal["hardware", "software", "patchstate"] = "hardware",
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> list[FactOut]:
    await _device_or_404(db, device_id)
    facts = await get_facts(db, kind, device_id, as_of=as_of, knew_at=knew_at)
    return [FactOut(**f) for f in facts]


@router.get("/{device_id}/snapshots/{kind}")
async def get_snapshot(
    device_id: uuid.UUID,
    kind: Literal["processes", "services"],
    db: DbSession,
    _user: CurrentUser,
) -> dict:
    await _device_or_404(db, device_id)
    snap = (
        await db.execute(
            select(DeviceSnapshot).where(
                DeviceSnapshot.device_id == device_id, DeviceSnapshot.kind == kind
            )
        )
    ).scalar_one_or_none()
    if snap is None:
        return {"payload": None, "updated_at": None}
    return {"payload": snap.payload, "updated_at": snap.updated_at.isoformat()}
