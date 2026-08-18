"""Organizations and sites can describe themselves.

The sync surface hands orgs and sites to an external system of record
(Nautobot SSoT), whose location model wants a description and a street
address. Nullable text, no backfill: an empty column means nobody has said
anything yet, which is true.

Revision ID: 0020
Revises: 0019
"""

import sqlalchemy as sa

from alembic import op

revision = "0020"
down_revision = "0019"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column("organizations", sa.Column("description", sa.Text(), nullable=True))
    op.add_column("sites", sa.Column("description", sa.Text(), nullable=True))
    op.add_column("sites", sa.Column("address", sa.Text(), nullable=True))


def downgrade() -> None:
    op.drop_column("sites", "address")
    op.drop_column("sites", "description")
    op.drop_column("organizations", "description")
