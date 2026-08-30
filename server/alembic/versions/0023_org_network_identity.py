"""An organization's policy for who provides 802.1X identities.

Set once at the top rather than pushed to each machine as a script, because the
common case is an organization that runs Active Directory everywhere and wants
one answer for the whole estate, not a decision repeated per host and forgotten
on the next machine somebody images.

Nullable, and nullable is the meaningful state rather than a gap: it means
nobody has decided, and the agent's own default applies. Filling it with "auto"
at migration time would look identical in the database and would erase the
difference between "we chose the cautious behaviour" and "nobody has thought
about this yet".

The agent PULLS this on its renewal timer rather than having it pushed. Pushing
loses exactly the machines that are switched off or travelling, which are the
ones most likely to be in an odd state already; the credential rotation that
shares this channel was moved from push to pull for that reason after a laptop
came back from a long weekend holding an expired secret.

Revision ID: 0023
Revises: 0022
"""

import sqlalchemy as sa

from alembic import op

revision = "0023"
down_revision = "0022"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column(
        "organizations",
        sa.Column("network_identity", sa.String(length=16), nullable=True),
    )
    # Refuse anything the agent would not understand, at the boundary rather
    # than at the far end. A value the agent cannot parse becomes "auto" with a
    # logged error on every machine, which is a fleet-wide non-event that looks
    # like the setting simply not working.
    op.create_check_constraint(
        "organizations_network_identity_known",
        "organizations",
        "network_identity IS NULL OR network_identity IN ('auto', 'always', 'never')",
    )


def downgrade() -> None:
    op.drop_constraint("organizations_network_identity_known", "organizations")
    op.drop_column("organizations", "network_identity")
