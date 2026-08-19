import uuid
from datetime import datetime

from pydantic import BaseModel, Field, model_validator

from everwas.models.alert import (
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


#: Config keys that are credentials. A webhook's HMAC `secret` lets anyone
#: forge signed deliveries; a gotify `token` posts as you.
SECRET_CONFIG_KEYS = frozenset({"secret", "token"})


class ChannelOut(BaseModel):
    """A channel, with its credentials REMOVED rather than masked.

    `config` used to be returned raw, so every browser session that listed
    channels received the webhook signing secret in plaintext. Masking it with
    a placeholder is worse than removing it: the obvious edit flow is GET,
    change the name, PUT the object back, which writes the literal mask over
    the real secret. Absent means absent, and `secrets_set` tells the UI a
    credential exists without saying what it is.

    Redaction is enforced by a validator rather than only by `redacted()`,
    because relying on every caller to pick the safe constructor is what failed:
    three routes existed, one used `redacted()`, and the other two shipped the
    credential to anyone with a login for months. A validator makes the unsafe
    object unconstructable, so `model_validate` straight off the ORM row is now
    safe too and there is no door left to pick the wrong one.
    """

    id: uuid.UUID
    name: str
    kind: ChannelKind
    config: dict
    enabled: bool
    secrets_set: list[str] = []

    model_config = {"from_attributes": True}

    @model_validator(mode="after")
    def _strip_credentials(self) -> "ChannelOut":
        raw = self.config or {}
        present = sorted(k for k in SECRET_CONFIG_KEYS if raw.get(k))
        if present:
            self.config = {k: v for k, v in raw.items() if k not in SECRET_CONFIG_KEYS}
            # Only widen: redacted() may have already reported these, and a
            # caller that constructed the object with an explicit list should
            # not lose it.
            self.secrets_set = sorted(set(self.secrets_set) | set(present))
        return self

    @classmethod
    def redacted(cls, channel) -> "ChannelOut":
        raw = channel.config or {}
        present = sorted(k for k in SECRET_CONFIG_KEYS if raw.get(k))
        return cls(
            id=channel.id,
            name=channel.name,
            kind=channel.kind,
            config={k: v for k, v in raw.items() if k not in SECRET_CONFIG_KEYS},
            enabled=channel.enabled,
            secrets_set=present,
        )


class OutboxOut(BaseModel):
    id: uuid.UUID
    channel_id: uuid.UUID
    status: OutboxStatus
    attempts: int
    last_error: str | None
    next_attempt_at: datetime

    model_config = {"from_attributes": True}
