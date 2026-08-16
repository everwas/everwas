import enum
import uuid
from datetime import datetime

from sqlalchemy import (
    ARRAY,
    BigInteger,
    DateTime,
    Enum,
    ForeignKey,
    String,
    Text,
    UniqueConstraint,
    func,
)
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base
from openrmm.models.device import OsFamily


class PatchSeverity(enum.StrEnum):
    critical = "critical"
    important = "important"
    moderate = "moderate"
    low = "low"
    unknown = "unknown"


class ApprovalDecision(enum.StrEnum):
    approved = "approved"
    declined = "declined"


class RebootPolicy(enum.StrEnum):
    never = "never"
    if_required = "if_required"
    always = "always"


class PatchJobStatus(enum.StrEnum):
    queued = "queued"
    running = "running"
    succeeded = "succeeded"
    failed = "failed"
    partial = "partial"
    cancelled = "cancelled"


class PatchCatalog(Base):
    """One row per distinct patch seen anywhere in the fleet.

    external_id is backend-native (WUA GUID, "pkg=version", macOS label), so it
    is only unique within an OS family.
    """

    __tablename__ = "patch_catalog"
    __table_args__ = (UniqueConstraint("os_family", "external_id", name="uq_patch_catalog_native"),)

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    os_family: Mapped[OsFamily] = mapped_column(Enum(OsFamily, name="os_family"))
    external_id: Mapped[str] = mapped_column(String(255))
    title: Mapped[str] = mapped_column(Text, default="")
    kind: Mapped[str] = mapped_column(String(32), default="other")
    severity: Mapped[PatchSeverity] = mapped_column(
        Enum(PatchSeverity, name="patch_severity"), default=PatchSeverity.unknown
    )
    kb_ids: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    cves: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    size_bytes: Mapped[int | None] = mapped_column(BigInteger, default=None)
    reboot_likely: Mapped[bool] = mapped_column(default=False)
    first_seen_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now()
    )


class PatchPolicy(Base):
    __tablename__ = "patch_policies"

    # Tenant boundary. Nullable and unenforced for now: see
    # openrmm.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"), default=None, index=True
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    target: Mapped[dict] = mapped_column(JSONB, default=dict)
    auto_approve_severities: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    # {"cron": "0 3 * * 6", "duration_min": 120, "tz": "UTC"}
    maintenance_window: Mapped[dict] = mapped_column(JSONB, default=dict)
    reboot_policy: Mapped[RebootPolicy] = mapped_column(
        Enum(RebootPolicy, name="reboot_policy"), default=RebootPolicy.never
    )
    enabled: Mapped[bool] = mapped_column(default=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class PatchApproval(Base):
    """A decision about one patch. Device-scoped when device_id is set,
    otherwise fleet-wide for the policy."""

    __tablename__ = "patch_approvals"
    __table_args__ = (UniqueConstraint("patch_id", "device_id", name="uq_patch_approval_scope"),)

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    patch_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("patch_catalog.id", ondelete="CASCADE"))
    device_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("devices.id", ondelete="CASCADE"), default=None
    )
    policy_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("patch_policies.id", ondelete="SET NULL"), default=None
    )
    decision: Mapped[ApprovalDecision] = mapped_column(
        Enum(ApprovalDecision, name="approval_decision")
    )
    decided_by: Mapped[str] = mapped_column(String(255), default="")
    decided_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class PatchJob(Base):
    """id IS the wire job_id, matching the script_runs convention."""

    __tablename__ = "patch_jobs"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    policy_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("patch_policies.id", ondelete="SET NULL"), default=None
    )
    external_ids: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    status: Mapped[PatchJobStatus] = mapped_column(
        Enum(PatchJobStatus, name="patch_job_status"), default=PatchJobStatus.queued, index=True
    )
    installed: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    failed: Mapped[dict] = mapped_column(JSONB, default=dict)
    reboot_required: Mapped[bool] = mapped_column(default=False)
    log: Mapped[str] = mapped_column(Text, default="")
    requested_by: Mapped[str | None] = mapped_column(String(255), default=None)
    queued_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    started_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    finished_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
