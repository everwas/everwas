"""agent credential rotation with an overlap window

Revision ID: 0010
Revises: 0009
Create Date: 2026-08-16

Rotation with one valid secret at a time is a lockout waiting to happen. The
server generates a secret, sends it to the agent, and commits. Every one of
those steps can be the last one that works: if the agent persists the secret
and the reply is lost, the server rolls back and the two disagree for ever.
The agent is not wrong and cannot re-enroll on its own, so recovery is a site
visit.

Keeping the previous hash valid for a grace window removes the failure mode
rather than narrowing it. Whichever secret the agent ends up holding, the next
reconnect succeeds, and the old one stops working on its own once the window
closes.

`prev_valid_until` is what makes the old secret expire; without it this is
just two permanent credentials.
"""

import sqlalchemy as sa

from alembic import op

revision = "0010"
down_revision = "0009"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column(
        "agent_credentials",
        sa.Column("prev_secret_hash", sa.String(length=64), nullable=True),
    )
    op.add_column(
        "agent_credentials",
        sa.Column("prev_valid_until", sa.DateTime(timezone=True), nullable=True),
    )
    # Partial: only rows in a rotation window are ever looked up this way, and
    # they are a vanishing fraction of the table.
    op.create_index(
        "ix_agent_credentials_rotating",
        "agent_credentials",
        ["prev_valid_until"],
        postgresql_where=sa.text("prev_valid_until IS NOT NULL"),
    )


def downgrade() -> None:
    op.drop_index("ix_agent_credentials_rotating", table_name="agent_credentials")
    op.drop_column("agent_credentials", "prev_valid_until")
    op.drop_column("agent_credentials", "prev_secret_hash")
