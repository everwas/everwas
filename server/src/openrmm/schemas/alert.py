import uuid
from datetime import datetime

from pydantic import BaseModel, Field

from openrmm.models.alert import (
    AlertState,
    ChannelKind,
    Metric,
    Operator,
    OutboxStatus,
    Severity,
)


class AlertRuleIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    metric: Metric
    operator: Operator = Operator.gt
    threshold: float = 0
    duration_s: int = Field(default=300, ge=0, le=86400)
    severity: Severity = Severity.warning
    target: dict = Field(default_factory=lambda: {"all": True})
    cooldown_s: int = Field(default=900, ge=0, le=86400)
    enabled: bool = True
    channel_ids: list[uuid.UUID] = Field(default_factory=list)


class AlertRuleOut(BaseModel):
    id: uuid.UUID
    name: str
    metric: Metric
    operator: Operator
    threshold: float
    duration_s: int
    severity: Severity
    target: dict
    cooldown_s: int
    enabled: bool
    channel_ids: list[uuid.UUID] = Field(default_factory=list)

    model_config = {"from_attributes": True}


class AlertOut(BaseModel):
    id: uuid.UUID
    rule_id: uuid.UUID
    device_id: uuid.UUID
    state: AlertState
    severity: Severity
    opened_at: datetime
    resolved_at: datetime | None
    acked_at: datetime | None
    acked_by: str | None
    last_value: float | None
    context: dict

    model_config = {"from_attributes": True}


class ChannelIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    kind: ChannelKind
    config: dict = Field(default_factory=dict)
    enabled: bool = True


class ChannelOut(BaseModel):
    id: uuid.UUID
    name: str
    kind: ChannelKind
    config: dict
    enabled: bool

    model_config = {"from_attributes": True}


class OutboxOut(BaseModel):
    id: uuid.UUID
    channel_id: uuid.UUID
    status: OutboxStatus
    attempts: int
    last_error: str | None
    next_attempt_at: datetime

    model_config = {"from_attributes": True}
