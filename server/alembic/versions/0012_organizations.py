"""organizations: the tenant boundary, added before it is needed

Revision ID: 0012
Revises: 0011
Create Date: 2026-08-16

Deliberately early. Retrofitting a tenant column onto a schema that has grown
for a year means a migration touching every table, a backfill per table, and a
window where half the code knows about organizations and half does not. Adding
it now makes turning multi-tenancy on later a change to QUERIES.

Nullable, with every existing row backfilled to a single "Default"
organization, so nothing behaves differently today. NOTHING FILTERS ON IT YET:
this is not an isolation boundary until the queries enforce it.

Only root-scoped tables get the column. Alerts, runs, patch jobs, facts,
telemetry and shell sessions reach an organization through their device or
script; copying it onto them is denormalization for query speed, which is a
decision for real query plans rather than for now.

ondelete=RESTRICT, not CASCADE: deleting an organization should be refused
while it still owns anything, not silently take the fleet with it.
"""

import uuid

import sqlalchemy as sa

from alembic import op

revision = "0012"
down_revision = "0011"
branch_labels = None
depends_on = None

DEFAULT_ORG_ID = uuid.UUID("00000000-0000-0000-0000-000000000001")

SCOPED = (
    "sites",
    "devices",
    "enrollment_tokens",
    "users",
    "api_keys",
    "scripts",
    "alert_rules",
    "notification_channels",
    "patch_policies",
)


def upgrade() -> None:
    op.create_table(
        "organizations",
        sa.Column("id", sa.UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(length=120), nullable=False, unique=True),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
    )
    op.execute(
        sa.text("INSERT INTO organizations (id, name) VALUES (:id, 'Default')").bindparams(
            sa.bindparam("id", DEFAULT_ORG_ID, type_=sa.UUID(as_uuid=True))
        )
    )

    for table in SCOPED:
        op.add_column(table, sa.Column("org_id", sa.UUID(as_uuid=True), nullable=True))
        op.create_foreign_key(
            f"fk_{table}_org", table, "organizations", ["org_id"], ["id"], ondelete="RESTRICT"
        )
        op.create_index(f"ix_{table}_org_id", table, ["org_id"])
        op.execute(
            sa.text(f"UPDATE {table} SET org_id = :id").bindparams(  # noqa: S608
                sa.bindparam("id", DEFAULT_ORG_ID, type_=sa.UUID(as_uuid=True))
            )
        )


def downgrade() -> None:
    for table in SCOPED:
        op.drop_index(f"ix_{table}_org_id", table_name=table)
        op.drop_constraint(f"fk_{table}_org", table, type_="foreignkey")
        op.drop_column(table, "org_id")
    op.drop_table("organizations")
