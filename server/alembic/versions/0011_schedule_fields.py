"""script_schedules gains the fields the agent actually needs

Revision ID: 0011
Revises: 0010
Create Date: 2026-08-16

The table has existed since 0001 but nothing ever wrote to it or read it: the
agent's scheduler was complete and the server never sent it a document, so no
schedule has ever fired. Wiring that up needs the rest of the entry the agent
parses.

`tz` is separate from cron on purpose. "0 2 * * *" means nothing without a
zone, and the agent fires on ITS clock while offline, so the zone has to
travel with the entry rather than being resolved server-side at push time.

`misfire_grace_s` bounds how late a missed fire may still run. Without it a
laptop that was closed at 02:00 starts its nightly job the moment it wakes at
09:00, in front of whoever opened it.
"""

import sqlalchemy as sa

from alembic import op

revision = "0011"
down_revision = "0010"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column(
        "script_schedules",
        sa.Column("name", sa.String(length=120), nullable=False, server_default=""),
    )
    op.add_column(
        "script_schedules",
        sa.Column("tz", sa.String(length=64), nullable=False, server_default="UTC"),
    )
    op.add_column(
        "script_schedules",
        sa.Column("misfire_grace_s", sa.Integer(), nullable=False, server_default="3600"),
    )
    op.add_column(
        "script_schedules",
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
    )
    # Agents are told about enabled schedules only, and the reconciliation
    # check runs on every heartbeat from every device.
    op.create_index(
        "ix_script_schedules_enabled",
        "script_schedules",
        ["enabled"],
        postgresql_where=sa.text("enabled"),
    )


def downgrade() -> None:
    op.drop_index("ix_script_schedules_enabled", table_name="script_schedules")
    for col in ("created_at", "misfire_grace_s", "tz", "name"):
        op.drop_column("script_schedules", col)
