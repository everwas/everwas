import enum
import uuid
from datetime import datetime

from sqlalchemy import ARRAY, DateTime, Enum, ForeignKey, Integer, String, Text, func, text
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base
from openrmm.models.org import DEFAULT_ORG_ID


class ShellKind(enum.StrEnum):
    bash = "bash"
    sh = "sh"
    zsh = "zsh"
    powershell = "powershell"
    pwsh = "pwsh"
    cmd = "cmd"
    python = "python"


class RunStatus(enum.StrEnum):
    queued = "queued"
    delivered = "delivered"
    running = "running"
    succeeded = "succeeded"
    failed = "failed"
    timeout = "timeout"
    cancelled = "cancelled"


class RunTrigger(enum.StrEnum):
    manual = "manual"
    schedule = "schedule"
    policy = "policy"
    mcp = "mcp"


class Script(Base):
    __tablename__ = "scripts"

    # Tenant boundary. Nullable and unenforced for now: see
    # openrmm.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    description: Mapped[str] = mapped_column(Text, default="")
    shell: Mapped[ShellKind] = mapped_column(Enum(ShellKind, name="shell_kind"))
    body: Mapped[str] = mapped_column(Text)
    os_filter: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    timeout_s: Mapped[int] = mapped_column(Integer, default=300)
    sha256: Mapped[str] = mapped_column(String(64))
    version: Mapped[int] = mapped_column(Integer, default=1)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    updated_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), onupdate=func.now()
    )
    updated_by: Mapped[str | None] = mapped_column(String(255), default=None)


class ScriptSchedule(Base):
    """A cron schedule pushed to the agents it targets.

    The agent fires these from its own local cache, so they run while it is
    offline and its clock, not the server's, decides when. Everything the
    agent needs to do that lives here and is mirrored into the sched.sync
    document (see openrmm.services.schedules).
    """

    __tablename__ = "script_schedules"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), default="")
    script_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("scripts.id", ondelete="CASCADE"))
    cron: Mapped[str] = mapped_column(String(120))
    # IANA name. The agent embeds tzdata (CGO_ENABLED=0 builds have no system
    # zoneinfo), so "America/Denver" resolves there whatever the host has.
    tz: Mapped[str] = mapped_column(String(64), default="UTC")
    target: Mapped[dict] = mapped_column(JSONB, default=dict)
    jitter_s: Mapped[int] = mapped_column(Integer, default=0)
    # How late a missed fire may still run. A machine that was asleep at 02:00
    # should not start a patch scan at 09:00 in front of whoever opened the
    # lid; past the grace the agent skips it and says so in an audit event.
    misfire_grace_s: Mapped[int] = mapped_column(Integer, default=3600)
    enabled: Mapped[bool] = mapped_column(default=True)
    last_run_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class ScriptRun(Base):
    """One row per device per run. id IS the job_id on the wire."""

    __tablename__ = "script_runs"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    script_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("scripts.id", ondelete="SET NULL"), default=None
    )
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    run_batch_id: Mapped[uuid.UUID | None] = mapped_column(default=None, index=True)
    trigger: Mapped[RunTrigger] = mapped_column(
        Enum(RunTrigger, name="run_trigger"), default=RunTrigger.manual
    )
    status: Mapped[RunStatus] = mapped_column(
        Enum(RunStatus, name="run_status"), default=RunStatus.queued, index=True
    )
    exit_code: Mapped[int | None] = mapped_column(Integer, default=None)
    stdout: Mapped[str] = mapped_column(Text, default="")
    stderr: Mapped[str] = mapped_column(Text, default="")
    truncated: Mapped[bool] = mapped_column(default=False)

    # Highest output sequence applied per stream, -1 meaning none yet. Read by
    # apply_job_output to make a redelivered chunk a no-op instead of a
    # duplicate; see migration 0015.
    stdout_seq: Mapped[int] = mapped_column(default=-1, server_default=text("-1"))
    stderr_seq: Mapped[int] = mapped_column(default=-1, server_default=text("-1"))
    requested_by: Mapped[str | None] = mapped_column(String(255), default=None)
    queued_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    started_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    finished_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)


class ShellSession(Base):
    __tablename__ = "shell_sessions"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    user_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL"), default=None
    )
    started_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    ended_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    close_reason: Mapped[str | None] = mapped_column(String(64), default=None)
    recording_path: Mapped[str | None] = mapped_column(String(255), default=None)
    bytes_in: Mapped[int] = mapped_column(Integer, default=0)
    bytes_out: Mapped[int] = mapped_column(Integer, default=0)
