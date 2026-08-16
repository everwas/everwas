"""job_outbox: transactional outbox for agent job dispatch

Revision ID: 0007
Revises: 0006
Create Date: 2026-08-16

"""

from collections.abc import Sequence

import sqlalchemy as sa
from sqlalchemy.dialects.postgresql import JSONB, UUID

from alembic import op

revision: str = "0007"
down_revision: str | None = "0006"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None

job_outbox_status = sa.Enum("pending", "published", "failed", "cancelled", name="job_outbox_status")


def upgrade() -> None:
    op.create_table(
        "job_outbox",
        # id IS the wire job_id (script_runs.id / patch_jobs.id), so the
        # primary key is also the dedup key sent as Nats-Msg-Id.
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("subject", sa.String(255), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False),
        sa.Column("payload", JSONB, nullable=False),
        sa.Column("status", job_outbox_status, nullable=False),
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
        sa.Column("published_at", sa.DateTime(timezone=True), nullable=True),
    )
    op.create_index("ix_job_outbox_status", "job_outbox", ["status"])
    op.create_index("ix_job_outbox_next_attempt", "job_outbox", ["next_attempt_at"])
    # The drainer's only query. Partial, so published rows cost nothing to keep.
    op.execute(
        "CREATE INDEX ix_job_outbox_due ON job_outbox (next_attempt_at) WHERE status = 'pending'"
    )


def downgrade() -> None:
    op.drop_table("job_outbox")
    job_outbox_status.drop(op.get_bind())
