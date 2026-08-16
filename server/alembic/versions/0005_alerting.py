"""alert_rules, alerts, notification_channels, rule_channels, notification_outbox

Revision ID: 0005
Revises: 0004
Create Date: 2026-08-15

"""

from collections.abc import Sequence

import sqlalchemy as sa
from sqlalchemy.dialects.postgresql import JSONB, UUID

from alembic import op

revision: str = "0005"
down_revision: str | None = "0004"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None

alert_metric = sa.Enum(
    "cpu",
    "memory",
    "disk",
    "heartbeat_missed",
    "service_down",
    "patch_overdue",
    name="alert_metric",
)
alert_operator = sa.Enum("gt", "lt", name="alert_operator")
alert_severity = sa.Enum("info", "warning", "critical", name="alert_severity")
alert_state = sa.Enum("firing", "acknowledged", "resolved", name="alert_state")
channel_kind = sa.Enum("email", "webhook", "ntfy", "gotify", name="channel_kind")
outbox_status = sa.Enum("pending", "sent", "failed", name="outbox_status")


def upgrade() -> None:
    op.create_table(
        "alert_rules",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(120), nullable=False, unique=True),
        sa.Column("metric", alert_metric, nullable=False),
        sa.Column("operator", alert_operator, nullable=False),
        sa.Column("threshold", sa.Numeric(10, 2), nullable=False),
        sa.Column("duration_s", sa.Integer(), nullable=False),
        sa.Column("severity", alert_severity, nullable=False),
        sa.Column("target", JSONB, nullable=False),
        sa.Column("cooldown_s", sa.Integer(), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )

    op.create_table(
        "alerts",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "rule_id",
            UUID(as_uuid=True),
            sa.ForeignKey("alert_rules.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("state", alert_state, nullable=False),
        sa.Column("severity", alert_severity, nullable=False),
        sa.Column(
            "opened_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column("resolved_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("acked_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("acked_by", sa.String(255), nullable=True),
        sa.Column("last_value", sa.Numeric(10, 2), nullable=True),
        sa.Column("context", JSONB, nullable=False),
    )
    op.create_index("ix_alerts_state", "alerts", ["state"])
    op.create_index("ix_alerts_device_opened", "alerts", ["device_id", "opened_at"])
    # THE dedup guarantee: at most one unresolved alert per (rule, device).
    op.execute(
        "CREATE UNIQUE INDEX uq_alerts_active ON alerts (rule_id, device_id) "
        "WHERE state <> 'resolved'"
    )

    op.create_table(
        "notification_channels",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(120), nullable=False, unique=True),
        sa.Column("kind", channel_kind, nullable=False),
        sa.Column("config", JSONB, nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )

    op.create_table(
        "rule_channels",
        sa.Column(
            "rule_id",
            UUID(as_uuid=True),
            sa.ForeignKey("alert_rules.id", ondelete="CASCADE"),
            primary_key=True,
        ),
        sa.Column(
            "channel_id",
            UUID(as_uuid=True),
            sa.ForeignKey("notification_channels.id", ondelete="CASCADE"),
            primary_key=True,
        ),
    )

    op.create_table(
        "notification_outbox",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "alert_id",
            UUID(as_uuid=True),
            sa.ForeignKey("alerts.id", ondelete="CASCADE"),
            nullable=True,
        ),
        sa.Column(
            "channel_id",
            UUID(as_uuid=True),
            sa.ForeignKey("notification_channels.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("payload", JSONB, nullable=False),
        sa.Column("status", outbox_status, nullable=False),
        sa.Column("attempts", sa.Integer(), nullable=False),
        sa.Column("last_error", sa.Text(), nullable=True),
        sa.Column(
            "next_attempt_at",
            sa.DateTime(timezone=True),
            server_default=sa.func.now(),
            nullable=False,
        ),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )
    op.create_index("ix_outbox_status", "notification_outbox", ["status"])
    op.create_index("ix_outbox_next_attempt", "notification_outbox", ["next_attempt_at"])


def downgrade() -> None:
    op.drop_table("notification_outbox")
    op.drop_table("rule_channels")
    op.drop_table("notification_channels")
    op.drop_table("alerts")
    op.drop_table("alert_rules")
    outbox_status.drop(op.get_bind())
    channel_kind.drop(op.get_bind())
    alert_state.drop(op.get_bind())
    alert_severity.drop(op.get_bind())
    alert_operator.drop(op.get_bind())
    alert_metric.drop(op.get_bind())
