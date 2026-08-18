"""audit_log carries its own organization instead of borrowing its subject's.

The log deliberately outlives the things it describes: delete_device writes
"device.deleted" BEFORE the row goes, precisely so the record survives the
machine. Authorizing the reader by loading that device therefore 404'd the
history of every deleted device, which is the history an incident is most
likely to want. Serving it unscoped was the alternative, and that hands one
tenant's operator identities to anyone who guesses a UUID.

Nullable, and it stays nullable. Readers filter with `org_id = :caller`, which
excludes NULL, so a writer that forgets to set it produces a row nobody can
read rather than a row everybody can. That is the direction this failure
should point.

Revision ID: 0018
Revises: 0017
"""

import sqlalchemy as sa

from alembic import op

revision = "0018"
down_revision = "0017"
branch_labels = None
depends_on = None

DEFAULT_ORG_ID = "00000000-0000-0000-0000-000000000001"


def upgrade() -> None:
    op.add_column("audit_log", sa.Column("org_id", sa.UUID(as_uuid=True), nullable=True))
    op.create_foreign_key(
        "fk_audit_log_org", "audit_log", "organizations", ["org_id"], ["id"], ondelete="RESTRICT"
    )
    # The reader filters on (org_id, target_id) and on (org_id, at); org_id
    # alone is what every one of those has in common.
    op.create_index("ix_audit_log_org_id", "audit_log", ["org_id"])

    # Derive what can be derived. target_id is a String(64) holding a UUID in
    # text form, so the join casts rather than comparing types.
    op.execute(
        """
        UPDATE audit_log a SET org_id = d.org_id
        FROM devices d
        WHERE a.org_id IS NULL AND a.target_type = 'device' AND a.target_id = d.id::text
        """
    )
    # An agent reporting its own event is the actor, not the target, and older
    # rows of that shape carry no target at all. Same device, same derivation.
    op.execute(
        """
        UPDATE audit_log a SET org_id = d.org_id
        FROM devices d
        WHERE a.org_id IS NULL AND a.actor_type = 'agent' AND a.actor_id = d.id::text
        """
    )
    # Everything else: a user, a site, an api key, or a device that has already
    # been deleted. Single-tenant until 0012, so the default organization is
    # the only answer that is true for existing installations, and leaving them
    # NULL would hide the entire existing trail from every reader.
    op.execute(
        f"UPDATE audit_log SET org_id = '{DEFAULT_ORG_ID}' WHERE org_id IS NULL"  # noqa: S608
    )


def downgrade() -> None:
    op.drop_index("ix_audit_log_org_id", table_name="audit_log")
    op.drop_constraint("fk_audit_log_org", "audit_log", type_="foreignkey")
    op.drop_column("audit_log", "org_id")
