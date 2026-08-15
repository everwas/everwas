import uuid
from datetime import datetime

from pydantic import BaseModel, Field

from openrmm.models.script import RunStatus, RunTrigger, ShellKind


class ScriptIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    description: str = ""
    shell: ShellKind
    body: str = Field(min_length=1)
    os_filter: list[str] = Field(default_factory=list)
    timeout_s: int = Field(default=300, ge=1, le=86400)


class ScriptOut(BaseModel):
    id: uuid.UUID
    name: str
    description: str
    shell: ShellKind
    body: str
    os_filter: list[str]
    timeout_s: int
    sha256: str
    version: int
    updated_at: datetime

    model_config = {"from_attributes": True}


class RunRequest(BaseModel):
    """Target selector: device_ids, tags, or all."""

    device_ids: list[uuid.UUID] = Field(default_factory=list)
    tags: list[str] = Field(default_factory=list)
    all: bool = False


class ScriptRunOut(BaseModel):
    id: uuid.UUID
    script_id: uuid.UUID | None
    device_id: uuid.UUID
    run_batch_id: uuid.UUID | None
    trigger: RunTrigger
    status: RunStatus
    exit_code: int | None
    stdout: str
    stderr: str
    truncated: bool
    requested_by: str | None
    queued_at: datetime
    started_at: datetime | None
    finished_at: datetime | None

    model_config = {"from_attributes": True}


class RunBatchOut(BaseModel):
    batch_id: uuid.UUID
    queued: int
    run_ids: list[uuid.UUID]
