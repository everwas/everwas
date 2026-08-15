"""scripts, script_schedules, script_runs, shell_sessions

Revision ID: 0004
Revises: 0003
Create Date: 2026-08-15

"""

from typing import Sequence, Union

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects.postgresql import JSONB, UUID

revision: str = "0004"
down_revision: Union[str, None] = "0003"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

shell_kind = sa.Enum(
    "bash", "sh", "zsh", "powershell", "pwsh", "cmd", "python", name="shell_kind"
)
run_status = sa.Enum(
    "queued", "delivered", "running", "succeeded", "failed", "timeout", "cancelled",
    name="run_status",
)
run_trigger = sa.Enum("manual", "schedule", "policy", "mcp", name="run_trigger")


def upgrade() -> None:
    op.create_table(
        "scripts",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(120), nullable=False, unique=True),
        sa.Column("description", sa.Text(), nullable=False),
        sa.Column("shell", shell_kind, nullable=False),
        sa.Column("body", sa.Text(), nullable=False),
        sa.Column("os_filter", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("timeout_s", sa.Integer(), nullable=False),
        sa.Column("sha256", sa.String(64), nullable=False),
        sa.Column("version", sa.Integer(), nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column(
            "updated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column("updated_by", sa.String(255), nullable=True),
    )

    op.create_table(
        "script_schedules",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "script_id",
            UUID(as_uuid=True),
            sa.ForeignKey("scripts.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("cron", sa.String(120), nullable=False),
        sa.Column("target", JSONB, nullable=False),
        sa.Column("jitter_s", sa.Integer(), nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False),
        sa.Column("last_run_at", sa.DateTime(timezone=True), nullable=True),
    )

    op.create_table(
        "script_runs",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "script_id",
            UUID(as_uuid=True),
            sa.ForeignKey("scripts.id", ondelete="SET NULL"),
            nullable=True,
        ),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("run_batch_id", UUID(as_uuid=True), nullable=True),
        sa.Column("trigger", run_trigger, nullable=False),
        sa.Column("status", run_status, nullable=False),
        sa.Column("exit_code", sa.Integer(), nullable=True),
        sa.Column("stdout", sa.Text(), nullable=False),
        sa.Column("stderr", sa.Text(), nullable=False),
        sa.Column("truncated", sa.Boolean(), nullable=False),
        sa.Column("requested_by", sa.String(255), nullable=True),
        sa.Column(
            "queued_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("finished_at", sa.DateTime(timezone=True), nullable=True),
    )
    op.create_index("ix_script_runs_run_batch_id", "script_runs", ["run_batch_id"])
    op.create_index("ix_script_runs_status", "script_runs", ["status"])
    op.create_index("ix_script_runs_device_queued", "script_runs", ["device_id", "queued_at"])

    op.create_table(
        "shell_sessions",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column(
            "user_id", UUID(as_uuid=True), sa.ForeignKey("users.id", ondelete="SET NULL"),
            nullable=True,
        ),
        sa.Column(
            "started_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column("ended_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("close_reason", sa.String(64), nullable=True),
        sa.Column("recording_path", sa.String(255), nullable=True),
        sa.Column("bytes_in", sa.Integer(), nullable=False),
        sa.Column("bytes_out", sa.Integer(), nullable=False),
    )


def downgrade() -> None:
    op.drop_table("shell_sessions")
    op.drop_table("script_runs")
    op.drop_table("script_schedules")
    op.drop_table("scripts")
    run_trigger.drop(op.get_bind())
    run_status.drop(op.get_bind())
    shell_kind.drop(op.get_bind())
