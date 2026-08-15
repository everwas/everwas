"""patch_catalog, patch_policies, patch_approvals, patch_jobs

Revision ID: 0006
Revises: 0005
Create Date: 2026-08-15

"""

from typing import Sequence, Union

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql
from sqlalchemy.dialects.postgresql import JSONB, UUID

revision: str = "0006"
down_revision: Union[str, None] = "0005"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

patch_severity = sa.Enum(
    "critical", "important", "moderate", "low", "unknown", name="patch_severity"
)
approval_decision = sa.Enum("approved", "declined", name="approval_decision")
reboot_policy = sa.Enum("never", "if_required", "always", name="reboot_policy")
patch_job_status = sa.Enum(
    "queued", "running", "succeeded", "failed", "partial", "cancelled", name="patch_job_status"
)
# os_family already exists from migration 0002. Reuse it: create_type=False is
# only honoured on the postgresql dialect ENUM, not on sa.Enum.
os_family = postgresql.ENUM("windows", "macos", "linux", name="os_family", create_type=False)


def upgrade() -> None:
    op.create_table(
        "patch_catalog",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("os_family", os_family, nullable=False),
        sa.Column("external_id", sa.String(255), nullable=False),
        sa.Column("title", sa.Text(), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False),
        sa.Column("severity", patch_severity, nullable=False),
        sa.Column("kb_ids", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("cves", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("size_bytes", sa.BigInteger(), nullable=True),
        sa.Column("reboot_likely", sa.Boolean(), nullable=False),
        sa.Column(
            "first_seen_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.UniqueConstraint("os_family", "external_id", name="uq_patch_catalog_native"),
    )

    op.create_table(
        "patch_policies",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column("name", sa.String(120), nullable=False, unique=True),
        sa.Column("target", JSONB, nullable=False),
        sa.Column("auto_approve_severities", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("maintenance_window", JSONB, nullable=False),
        sa.Column("reboot_policy", reboot_policy, nullable=False),
        sa.Column("enabled", sa.Boolean(), nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
    )

    op.create_table(
        "patch_approvals",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "patch_id",
            UUID(as_uuid=True),
            sa.ForeignKey("patch_catalog.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=True,
        ),
        sa.Column(
            "policy_id",
            UUID(as_uuid=True),
            sa.ForeignKey("patch_policies.id", ondelete="SET NULL"),
            nullable=True,
        ),
        sa.Column("decision", approval_decision, nullable=False),
        sa.Column("decided_by", sa.String(255), nullable=False),
        sa.Column(
            "decided_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.UniqueConstraint("patch_id", "device_id", name="uq_patch_approval_scope"),
    )

    op.create_table(
        "patch_jobs",
        sa.Column("id", UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "device_id",
            UUID(as_uuid=True),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column(
            "policy_id",
            UUID(as_uuid=True),
            sa.ForeignKey("patch_policies.id", ondelete="SET NULL"),
            nullable=True,
        ),
        sa.Column("external_ids", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("status", patch_job_status, nullable=False),
        sa.Column("installed", sa.ARRAY(sa.String()), nullable=False),
        sa.Column("failed", JSONB, nullable=False),
        sa.Column("reboot_required", sa.Boolean(), nullable=False),
        sa.Column("log", sa.Text(), nullable=False),
        sa.Column("requested_by", sa.String(255), nullable=True),
        sa.Column(
            "queued_at", sa.DateTime(timezone=True), server_default=sa.func.now(), nullable=False
        ),
        sa.Column("started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("finished_at", sa.DateTime(timezone=True), nullable=True),
    )
    op.create_index("ix_patch_jobs_status", "patch_jobs", ["status"])
    op.create_index("ix_patch_jobs_device", "patch_jobs", ["device_id", "queued_at"])


def downgrade() -> None:
    op.drop_table("patch_jobs")
    op.drop_table("patch_approvals")
    op.drop_table("patch_policies")
    op.drop_table("patch_catalog")
    patch_job_status.drop(op.get_bind())
    reboot_policy.drop(op.get_bind())
    approval_decision.drop(op.get_bind())
    patch_severity.drop(op.get_bind())
