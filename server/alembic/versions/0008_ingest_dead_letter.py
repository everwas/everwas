"""ingest_dead_letter

Revision ID: 0008
Revises: 0007
Create Date: 2026-08-16

Consumers now have a max_deliver limit, so a poison message stops redelivering
forever. Without somewhere to put it, that limit would silently DROP the
message instead. This table is where a message that could never be processed
goes, with the payload and the traceback, so an operator can answer "what did
we lose, and why".
"""

from collections.abc import Sequence

import sqlalchemy as sa
from sqlalchemy.dialects.postgresql import UUID

from alembic import op

revision: str = "0008"
down_revision: str | None = "0007"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    op.create_table(
        "ingest_dead_letter",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False),
        sa.Column("stream", sa.String(64), nullable=False),
        sa.Column("durable", sa.String(64), nullable=False),
        sa.Column("subject", sa.String(255), nullable=False),
        sa.Column("payload", sa.LargeBinary(), nullable=False),
        sa.Column("delivered", sa.Integer(), nullable=False),
        sa.Column("error", sa.Text(), nullable=False),
    )
    op.create_index("ix_ingest_dead_letter_subject", "ingest_dead_letter", ["subject"])
    op.create_index("ix_ingest_dead_letter_at", "ingest_dead_letter", ["at"])


def downgrade() -> None:
    op.drop_table("ingest_dead_letter")
