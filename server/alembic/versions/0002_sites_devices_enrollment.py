"""sites, devices, enrollment_tokens, agent_credentials

Revision ID: 0002
Revises: 0001
Create Date: 2026-08-15

"""

from typing import Sequence, Union

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects.postgresql import UUID

revision: str = "0002"
down_revision: Union[str, None] = "0001"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

os_family = sa.Enum("windows", "macos", "linux", name="os_family")
device_status = sa.Enum("enrolled", "active", "offline", "retired", name="device_status")


def upgrade() -> None:
    op.create_table(
        "sites",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(120), nullable=False, unique=True),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )

    op.create_table(
        "devices",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "site_id",
            UUID(as_uuid=True),
            sa.ForeignKey("sites.id", ondelete="SET NULL"),
            nullable=True,
        ),
        sa.Column("hostname", sa.String(255), nullable=False),
        sa.Column("os_family", os_family, nullable=False),
        sa.Column("os_version", sa.String(120), nullable=False),
        sa.Column("arch", sa.String(32), nullable=False),
        sa.Column("agent_version", sa.String(64), nullable=False),
        sa.Column("status", device_status, nullable=False),
        sa.Column("tags", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("last_heartbeat_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column(
            "enrolled_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )
    op.create_index("ix_devices_hostname", "devices", ["hostname"])
    op.create_index("ix_devices_status", "devices", ["status"])

    op.create_table(
        "enrollment_tokens",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("token_hash", sa.String(64), nullable=False, unique=True),
        sa.Column(
            "site_id",
            UUID(as_uuid=True),
            sa.ForeignKey("sites.id", ondelete="CASCADE"),
            nullable=True,
        ),
        sa.Column("max_uses", sa.Integer(), nullable=False),
        sa.Column("uses", sa.Integer(), nullable=False),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_by", sa.String(255), nullable=True),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )

    op.create_table(
        "agent_credentials",
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            primary_key=True,
        ),
        sa.Column("secret_hash", sa.String(64), nullable=False),
        sa.Column(
            "rotated_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )


def downgrade() -> None:
    op.drop_table("agent_credentials")
    op.drop_table("enrollment_tokens")
    op.drop_table("devices")
    op.drop_table("sites")
    device_status.drop(op.get_bind())
    os_family.drop(op.get_bind())
