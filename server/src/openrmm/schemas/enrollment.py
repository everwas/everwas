import uuid
from datetime import datetime

from pydantic import BaseModel, Field

from openrmm.models.device import OsFamily


class EnrollRequest(BaseModel):
    token: str = Field(min_length=8)
    hostname: str = Field(min_length=1, max_length=255)
    os_family: OsFamily
    os_version: str = ""
    arch: str = ""
    agent_version: str = ""
    fingerprint: dict = Field(default_factory=dict)


class EnrollResponse(BaseModel):
    agent_id: uuid.UUID
    agent_secret: str
    nats_url: str


class EnrollmentTokenOut(BaseModel):
    id: uuid.UUID
    token: str  # shown exactly once, at creation
    site_id: uuid.UUID | None
    max_uses: int
    expires_at: datetime | None
