"""Record which certificate a device says it is actually holding.

We already know what we last ISSUED each device. That is a different fact from
what the machine has, and the two drift apart in three ways that are currently
invisible: a renewal that half-failed, where the certificate was issued and the
device never managed to save it; a machine restored from a backup image or
cloned from a template, which comes back holding something retired months ago
and authenticates with it perfectly happily; and material deleted by hand.

Each of those surfaces later as an authentication failure nobody can account
for, because the only record anyone can consult says the device was issued a
good certificate, which is true and not the question.

Nullable, and it stays nullable. An empty value means "this agent has not told
us", which is the honest state for an agent too old to report and for every
deployment that does not use 802.1X. Defaulting it to anything would invent a
belief we do not hold.

Revision ID: 0019
Revises: 0018
"""

import sqlalchemy as sa

from alembic import op

revision = "0019"
down_revision = "0018"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.add_column(
        "devices",
        # Lowercase hex, the same form services/certificates.py records, so the
        # two can be compared without either side knowing how the other
        # formats a big integer.
        sa.Column("reported_cert_serial", sa.String(length=64), nullable=True),
    )
    op.add_column(
        "devices",
        sa.Column("reported_cert_not_after", sa.DateTime(timezone=True), nullable=True),
    )
    op.add_column(
        "devices",
        # When the device last told us. Distinguishes "reported nothing five
        # minutes ago", which means the material is genuinely gone, from
        # "never reported", which means the agent is too old to say.
        sa.Column("reported_cert_at", sa.DateTime(timezone=True), nullable=True),
    )
    # The drift query filters on devices whose reported serial differs from
    # their newest issued one, so it reads this column for most of the fleet.
    op.create_index(
        "ix_devices_reported_cert_serial", "devices", ["reported_cert_serial"]
    )


def downgrade() -> None:
    op.drop_index("ix_devices_reported_cert_serial", table_name="devices")
    op.drop_column("devices", "reported_cert_at")
    op.drop_column("devices", "reported_cert_not_after")
    op.drop_column("devices", "reported_cert_serial")
