import enum
import uuid
from datetime import datetime

from sqlalchemy import (
    DateTime,
    Enum,
    ForeignKey,
    Integer,
    Numeric,
    String,
    Text,
    func,
)
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class Metric(enum.StrEnum):
    cpu = "cpu"
    memory = "memory"
    disk = "disk"
    heartbeat_missed = "heartbeat_missed"
    service_down = "service_down"
    patch_overdue = "patch_overdue"


class Operator(enum.StrEnum):
    gt = "gt"
    lt = "lt"


class Severity(enum.StrEnum):
    info = "info"
    warning = "warning"
    critical = "critical"


class AlertState(enum.StrEnum):
    firing = "firing"
    acknowledged = "acknowledged"
    resolved = "resolved"


class ChannelKind(enum.StrEnum):
    email = "email"
    webhook = "webhook"
    ntfy = "ntfy"
    gotify = "gotify"


class OutboxStatus(enum.StrEnum):
    pending = "pending"
    sent = "sent"
    failed = "failed"


class AlertRule(Base):
    __tablename__ = "alert_rules"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    metric: Mapped[Metric] = mapped_column(Enum(Metric, name="alert_metric"))
    operator: Mapped[Operator] = mapped_column(
        Enum(Operator, name="alert_operator"), default=Operator.gt
    )
    threshold: Mapped[float] = mapped_column(Numeric(10, 2), default=0)
    duration_s: Mapped[int] = mapped_column(Integer, default=300)
    severity: Mapped[Severity] = mapped_column(
        Enum(Severity, name="alert_severity"), default=Severity.warning
    )
    # {device_ids: [...]} | {tags: [...]} | {all: true}
    target: Mapped[dict] = mapped_column(JSONB, default=dict)
    cooldown_s: Mapped[int] = mapped_column(Integer, default=900)
    enabled: Mapped[bool] = mapped_column(default=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class Alert(Base):
    """One row per (rule, device) incident.

    Migration 0005 adds a partial unique index on (rule_id, device_id) WHERE
    state <> 'resolved' — that constraint, not application logic, is what makes
    a duplicate fire impossible.
    """

    __tablename__ = "alerts"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    rule_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("alert_rules.id", ondelete="CASCADE"))
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    state: Mapped[AlertState] = mapped_column(
        Enum(AlertState, name="alert_state"), default=AlertState.firing, index=True
    )
    severity: Mapped[Severity] = mapped_column(Enum(Severity, name="alert_severity"))
    opened_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    resolved_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    acked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    acked_by: Mapped[str | None] = mapped_column(String(255), default=None)
    last_value: Mapped[float | None] = mapped_column(Numeric(10, 2), default=None)
    context: Mapped[dict] = mapped_column(JSONB, default=dict)


class NotificationChannel(Base):
    __tablename__ = "notification_channels"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    kind: Mapped[ChannelKind] = mapped_column(Enum(ChannelKind, name="channel_kind"))
    # email: {to: [...]}; webhook: {url, secret?}; ntfy/gotify: {url, topic/token}
    config: Mapped[dict] = mapped_column(JSONB, default=dict)
    enabled: Mapped[bool] = mapped_column(default=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class RuleChannel(Base):
    __tablename__ = "rule_channels"

    rule_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("alert_rules.id", ondelete="CASCADE"), primary_key=True
    )
    channel_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("notification_channels.id", ondelete="CASCADE"), primary_key=True
    )


class NotificationOutbox(Base):
    """Transactional outbox: alerts and their notifications commit together,
    delivery happens out of band with retries."""

    __tablename__ = "notification_outbox"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    alert_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("alerts.id", ondelete="CASCADE"), default=None
    )
    channel_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("notification_channels.id", ondelete="CASCADE")
    )
    payload: Mapped[dict] = mapped_column(JSONB)
    status: Mapped[OutboxStatus] = mapped_column(
        Enum(OutboxStatus, name="outbox_status"), default=OutboxStatus.pending, index=True
    )
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    last_error: Mapped[str | None] = mapped_column(Text, default=None)
    next_attempt_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), index=True
    )
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
