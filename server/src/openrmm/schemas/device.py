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
