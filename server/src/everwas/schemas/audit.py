import uuid
from datetime import datetime

from pydantic import BaseModel, ConfigDict

from everwas.models.audit import ActorType


class AuditEntryOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    at: datetime
    actor_type: ActorType
    actor_id: str | None
    action: str
    target_type: str | None
    target_id: str | None
    detail: dict | None


class AuditPage(BaseModel):
    """Keyset pagination. An offset over an append-only table that is being
    written to while you page through it silently skips and repeats rows."""

    entries: list[AuditEntryOut]
    has_more: bool
    next_before: datetime | None
