"""SQLAlchemy models. Import all so Base.metadata sees every table (Alembic autogenerate)."""

from everwas.models.alert import (
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
from everwas.models.api_key import ApiKey
from everwas.models.audit import AuditLog
from everwas.models.certificate import DeviceCertificate
from everwas.models.deadletter import IngestDeadLetter
from everwas.models.device import AgentCredential, Device, EnrollmentToken, Site
from everwas.models.facts import FACT_TABLES, FactHardware, FactPatchState, FactSoftware
from everwas.models.job_outbox import JobOutbox, JobOutboxStatus
from everwas.models.org import DEFAULT_ORG_ID, Organization
from everwas.models.patch import (
    ApprovalDecision,
    PatchApproval,
    PatchCatalog,
    PatchJob,
    PatchJobStatus,
    PatchPolicy,
    PatchSeverity,
    RebootPolicy,
)
from everwas.models.script import (
    RunStatus,
    RunTrigger,
    Script,
    ScriptRun,
    ScriptSchedule,
    ShellKind,
    ShellSession,
)
from everwas.models.session import Session
from everwas.models.telemetry import (
    DeviceSnapshot,
    DeviceStatusLatest,
    telemetry_disks,
    telemetry_metrics,
)
from everwas.models.user import User

__all__ = [
    "DeviceCertificate",
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
    "Organization",
    "DEFAULT_ORG_ID",
    "ScriptSchedule",
    "ShellKind",
    "ShellSession",
    "Session",
    "Site",
    "User",
    "telemetry_disks",
    "telemetry_metrics",
]
