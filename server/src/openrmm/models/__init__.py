"""SQLAlchemy models. Import all so Base.metadata sees every table (Alembic autogenerate)."""

from openrmm.models.alert import (
    Alert,
    AlertRule,
    AlertState,
    ChannelKind,
    Metric,
    NotificationChannel,
    NotificationOutbox,
    Operator,
    OutboxStatus,
    RuleChannel,
    Severity,
)
from openrmm.models.api_key import ApiKey
from openrmm.models.audit import AuditLog
from openrmm.models.deadletter import IngestDeadLetter
from openrmm.models.device import AgentCredential, Device, EnrollmentToken, Site
from openrmm.models.facts import FACT_TABLES, FactHardware, FactPatchState, FactSoftware
from openrmm.models.job_outbox import JobOutbox, JobOutboxStatus
from openrmm.models.patch import (
    ApprovalDecision,
    PatchApproval,
    PatchCatalog,
    PatchJob,
    PatchJobStatus,
    PatchPolicy,
    PatchSeverity,
    RebootPolicy,
)
from openrmm.models.script import (
    RunStatus,
    RunTrigger,
    Script,
    ScriptRun,
    ScriptSchedule,
    ShellKind,
    ShellSession,
)
from openrmm.models.session import Session
from openrmm.models.telemetry import (
    DeviceSnapshot,
    DeviceStatusLatest,
    telemetry_disks,
    telemetry_metrics,
)
from openrmm.models.user import User

__all__ = [
    "FACT_TABLES",
    "AgentCredential",
    "Alert",
    "AlertRule",
    "AlertState",
    "ApiKey",
    "ChannelKind",
    "Metric",
    "NotificationChannel",
    "NotificationOutbox",
    "Operator",
    "OutboxStatus",
    "RuleChannel",
    "Severity",
    "AuditLog",
    "Device",
    "IngestDeadLetter",
    "DeviceSnapshot",
    "DeviceStatusLatest",
    "EnrollmentToken",
    "FactHardware",
    "FactPatchState",
    "FactSoftware",
    "JobOutbox",
    "JobOutboxStatus",
    "ApprovalDecision",
    "PatchApproval",
    "PatchCatalog",
    "PatchJob",
    "PatchJobStatus",
    "PatchPolicy",
    "PatchSeverity",
    "RebootPolicy",
    "RunStatus",
    "RunTrigger",
    "Script",
    "ScriptRun",
    "ScriptSchedule",
    "ShellKind",
    "ShellSession",
    "Session",
    "Site",
    "User",
    "telemetry_disks",
    "telemetry_metrics",
]
