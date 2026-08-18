"""Issued device certificates, so they can be listed, monitored and revoked.

A CA that signs and forgets cannot answer the two questions that matter
operationally: which devices are about to lose network access, and what do I
publish when one is stolen.

Expiry monitoring is the important half. For 802.1X an expired certificate
locks a device off the network, and a device that cannot reach the network
cannot be repaired remotely: it is a physical visit. Certificates are renewed at
half life specifically so there are weeks of alarms first, and alarms need a
table to look at.

Revision ID: 0017
Revises: 0016
"""

import sqlalchemy as sa
from sqlalchemy.dialects import postgresql

from alembic import op

revision = "0017"
down_revision = "0016"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.create_table(
        "device_certificates",
        # The serial IS the identity: it is what a CRL revokes by, and what a
        # RADIUS log line names. Stored as text because an X.509 serial is a
        # 20-byte integer that does not fit in bigint.
        sa.Column("serial", sa.String(64), primary_key=True),
        sa.Column(
            "device_id",
            postgresql.UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
            index=True,
        ),
        sa.Column("common_name", sa.Text(), nullable=False),
        # The certificate itself, so it can be re-served and inspected without
        # asking the device. Never the private key: that is generated on the
        # endpoint and never transmitted.
        sa.Column("certificate_pem", sa.Text(), nullable=False),
        sa.Column("fingerprint_sha256", sa.String(64), nullable=False, index=True),
        sa.Column("not_before", sa.DateTime(timezone=True), nullable=False),
        sa.Column("not_after", sa.DateTime(timezone=True), nullable=False, index=True),
        sa.Column("issued_at", sa.DateTime(timezone=True), server_default=sa.func.now()),
        # Revocation is recorded here and published through the CRL. Nullable
        # because most certificates are never revoked; they simply expire.
        sa.Column("revoked_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("revocation_reason", sa.String(64), nullable=True),
    )
    # The query the renewal sweep and the expiry alarm both run: what is live,
    # and what is close to the edge.
    op.execute(
        """
        CREATE INDEX ix_device_certificates_live
            ON device_certificates (not_after)
            WHERE revoked_at IS NULL
        """
    )


def downgrade() -> None:
    op.drop_table("device_certificates")
