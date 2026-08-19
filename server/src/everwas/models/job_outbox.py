"""Transactional outbox for agent job dispatch.

A job row (script_runs, patch_jobs) and its outbox row are written in the same
transaction. Nothing is published to JetStream until that transaction commits,
so the fleet can never be executing work the database has no record of.

Same shape as models.alert.NotificationOutbox, which is where this pattern was
already proven in this codebase.
"""

import enum
import uuid
from datetime import datetime

from sqlalchemy import (
    DateTime,
    Enum,
    ForeignKey,
    Integer,
    String,
    Text,
    func,
)
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base

# Wire `kind` values, stored verbatim so the drainer can put them straight into
# the envelope. Not a DB enum: the agent's job kinds are a wire contract
# (docs/nats-subjects.md), not a schema we want to migrate for.
KIND_SCRIPT_RUN = "script.run"
KIND_PATCH_INSTALL = "patch.install"
KIND_PATCH_SCAN = "patch.scan"


class JobOutboxStatus(enum.StrEnum):
    pending = "pending"
    published = "published"
    failed = "failed"
    cancelled = "cancelled"


class JobOutbox(Base):
    """One row per job awaiting delivery to an agent.

    `id` IS the wire job_id, matching the script_runs / patch_jobs convention.
    That makes the primary key the dedup key too: one delivery per job, and the
    same value rides as `Nats-Msg-Id` so a redelivery is dropped by JetStream.
    """

    __tablename__ = "job_outbox"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    # Resolved at queue time and stored, so delivery never depends on subject
    # construction agreeing with what the operator was told.
    subject: Mapped[str] = mapped_column(String(255))
    kind: Mapped[str] = mapped_column(String(32))
    payload: Mapped[dict] = mapped_column(JSONB, default=dict)
    status: Mapped[JobOutboxStatus] = mapped_column(
        Enum(JobOutboxStatus, name="job_outbox_status"),
        default=JobOutboxStatus.pending,
        index=True,
    )
    attempts: Mapped[int] = mapped_column(Integer, default=0)
    last_error: Mapped[str | None] = mapped_column(Text, default=None)
    next_attempt_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), index=True
    )
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    published_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
