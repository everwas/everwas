import uuid
from datetime import datetime

from pydantic import BaseModel, ConfigDict, Field, field_validator

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


class ScheduleIn(BaseModel):
    """A cron schedule. Validated here so a bad one is a 422 to the operator
    rather than an entry every agent in the fleet quietly refuses."""

    name: str = Field(min_length=1, max_length=120)
    script_id: uuid.UUID
    cron: str = Field(min_length=1, max_length=120)
    tz: str = "UTC"
    target: dict = Field(default_factory=lambda: {"all": True})
    jitter_s: int = Field(default=0, ge=0, le=3600)
    misfire_grace_s: int = Field(default=3600, ge=0, le=86400)
    enabled: bool = True

    @field_validator("cron")
    @classmethod
    def _cron_parses(cls, v: str) -> str:
        # croniter, not the agent's parser, but both accept standard 5-field
        # cron plus @descriptors. The agent re-validates and reports anything
        # it still refuses in the sched.sync reply.
        from croniter import croniter

        if not croniter.is_valid(v):
            raise ValueError(f"{v!r} is not a valid cron expression")
        return v

    @field_validator("target")
    @classmethod
    def _target_names_one_selector(cls, v: dict) -> dict:
        # A schedule's target is evaluated for EVERY device on EVERY heartbeat,
        # off one list shared by the whole fleet. An unusable one used to save
        # with a 201 and then raise inside the reconciler, so no agent anywhere
        # received a schedule document until somebody found the row. Two
        # selectors is the plausible mistake: RunRequest always sends all three
        # keys, so a target copied out of a run request has device_ids and all.
        from openrmm.services.jobs import TargetError, validate_target

        try:
            validate_target(v)
        except TargetError as exc:
            raise ValueError(str(exc)) from exc
        return v

    @field_validator("tz")
    @classmethod
    def _tz_exists(cls, v: str) -> str:
        from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

        try:
            ZoneInfo(v)
        except (ZoneInfoNotFoundError, ValueError) as exc:
            raise ValueError(f"unknown timezone {v!r}") from exc
        return v


class ScheduleOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    name: str
    script_id: uuid.UUID
    cron: str
    tz: str
    target: dict
    jitter_s: int
    misfire_grace_s: int
    enabled: bool
    last_run_at: datetime | None
