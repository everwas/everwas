import uuid
from datetime import datetime

from pydantic import BaseModel

from openrmm.models.device import DeviceStatus, OsFamily


class DeviceOut(BaseModel):
    id: uuid.UUID
    hostname: str
    os_family: OsFamily
    os_version: str
    arch: str
    agent_version: str
    status: DeviceStatus
    tags: list[str]
    last_heartbeat_at: datetime | None
    enrolled_at: datetime

    model_config = {"from_attributes": True}


class DeviceDetailOut(DeviceOut):
    cpu_pct: float | None = None
    mem_pct: float | None = None
    worst_disk_pct: float | None = None


class TelemetryPoint(BaseModel):
    ts: datetime
    cpu_pct: float | None
    mem_pct: float | None
    load1: float | None


class FactOut(BaseModel):
    fact_key: str
    payload: dict
    valid_from: datetime | None
    valid_to: datetime | None
    source: str
