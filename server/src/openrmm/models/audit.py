import enum
import uuid
from datetime import datetime

from sqlalchemy import DateTime, Enum, Index, String, func
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class ActorType(enum.StrEnum):
    user = "user"
    api_key = "api_key"
    agent = "agent"
    system = "system"


class AuditLog(Base):
    """Append-only. Never updated, never deleted by application code."""

    __tablename__ = "audit_log"
    __table_args__ = (Index("ix_audit_log_at_brin", "at", postgresql_using="brin"),)

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    actor_type: Mapped[ActorType] = mapped_column(Enum(ActorType, name="actor_type"))
    actor_id: Mapped[str | None] = mapped_column(String(64), default=None)
    action: Mapped[str] = mapped_column(String(120), index=True)
    target_type: Mapped[str | None] = mapped_column(String(64), default=None)
    target_id: Mapped[str | None] = mapped_column(String(64), default=None)
    detail: Mapped[dict | None] = mapped_column(JSONB, default=None)
