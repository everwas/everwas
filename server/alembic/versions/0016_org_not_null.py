"""Make org_id NOT NULL, so nothing can exist outside an organization.

The column was added nullable and unenforced, deliberately, so that turning
multi-tenancy on later would be a change to queries rather than a migration
across the schema. That was the right call for the schema and it left one
trap: enroll_device never set org_id, so every device enrolled since was
NULL-org. A filter written as `WHERE org_id = :caller` silently excludes those
rather than failing loudly, so switching the boundary on would have quietly
hidden the entire existing fleet from everyone.

NOT NULL is what removes the trap. A row that belongs to no organization is
now unrepresentable, so the failure mode becomes an insert error at write time
rather than a device that is invisible to every query.

Revision ID: 0016
Revises: 0015
"""

import sqlalchemy as sa

from alembic import op

revision = "0016"
down_revision = "0015"
branch_labels = None
depends_on = None

DEFAULT_ORG_ID = "00000000-0000-0000-0000-000000000001"

# Every root-scoped table. Children (alerts, runs, patch jobs, facts,
# telemetry, shell sessions) reach an organization through their device or
# script and deliberately do not carry their own copy.
SCOPED_TABLES = (
    "users",
    "api_keys",
    "sites",
    "devices",
    "enrollment_tokens",
    "scripts",
    "alert_rules",
    "notification_channels",
    "patch_policies",
)


def upgrade() -> None:
    # The default organization must exist before anything can reference it.
    # 0012 created it, but a database restored from before that, or one where
    # it was removed, would fail the backfill with a foreign key error rather
    # than an obvious message.
    op.execute(
        f"""
        INSERT INTO organizations (id, name)
        VALUES ('{DEFAULT_ORG_ID}', 'Default')
        ON CONFLICT (id) DO NOTHING
        """
    )
    for table in SCOPED_TABLES:
        op.execute(
            f"UPDATE {table} SET org_id = '{DEFAULT_ORG_ID}' WHERE org_id IS NULL"  # noqa: S608
        )
        op.alter_column(table, "org_id", existing_type=sa.UUID(), nullable=False)


def downgrade() -> None:
    for table in SCOPED_TABLES:
        op.alter_column(table, "org_id", existing_type=sa.UUID(), nullable=True)
