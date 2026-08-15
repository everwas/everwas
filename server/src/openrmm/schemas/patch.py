import uuid
from datetime import datetime

from pydantic import BaseModel, Field

from openrmm.models.device import OsFamily
from openrmm.models.patch import PatchJobStatus, PatchSeverity, RebootPolicy


class PatchOut(BaseModel):
    id: uuid.UUID
    os_family: OsFamily
    external_id: str
    title: str
    kind: str
    severity: PatchSeverity
    kb_ids: list[str]
    cves: list[str]
    size_bytes: int | None
    reboot_likely: bool

    model_config = {"from_attributes": True}


class DevicePatchOut(PatchOut):
    """A patch as it applies to one device: pending, approved, or installed."""

    approved: bool = False
    unsupported: bool = False
    detail: str = ""


class ApprovalRequest(BaseModel):
    patch_ids: list[uuid.UUID] = Field(min_length=1)
    device_id: uuid.UUID | None = None
    decision: str = "approved"


class DeployRequest(BaseModel):
    device_id: uuid.UUID
    # empty means "everything currently approved for this device"
    external_ids: list[str] = Field(default_factory=list)


class PatchJobOut(BaseModel):
    id: uuid.UUID
    device_id: uuid.UUID
    external_ids: list[str]
    status: PatchJobStatus
    installed: list[str]
    failed: dict
    reboot_required: bool
    requested_by: str | None
    queued_at: datetime
    finished_at: datetime | None

    model_config = {"from_attributes": True}


class PatchPolicyIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    target: dict = Field(default_factory=lambda: {"all": True})
    auto_approve_severities: list[str] = Field(default_factory=list)
    maintenance_window: dict = Field(default_factory=dict)
    reboot_policy: RebootPolicy = RebootPolicy.never
    enabled: bool = True


class PatchPolicyOut(PatchPolicyIn):
    id: uuid.UUID

    model_config = {"from_attributes": True}
