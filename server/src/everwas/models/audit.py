import enum
import uuid
from datetime import datetime

from sqlalchemy import DateTime, Enum, ForeignKey, Index, String, func
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base


class ActorType(enum.StrEnum):
    user = "user"
    api_key = "api_key"
    agent = "agent"
    system = "system"


class AuditLog(Base):
    """Append-only. Never updated, never deleted by application code."""

    __tablename__ = "audit_log"
    __table_args__ = (Index("ix_audit_log_at_brin", "at", postgresql_using="brin"),)

    # Its OWN tenant, not one reached through the thing it describes. The log
    # outlives its subjects on purpose (delete_device writes the entry before
    # the row goes), so a reader that authorized through the device could not
    # show the history of a deleted one.
    #
    # Nullable, and no default: unlike every other table, a default here would
    # quietly file an entry under the wrong tenant whenever a writer forgot.
    # NULL is unreadable instead, because readers filter `org_id = :caller`.
    org_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=None,
        index=True,
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    actor_type: Mapped[ActorType] = mapped_column(Enum(ActorType, name="actor_type"))
    actor_id: Mapped[str | None] = mapped_column(String(64), default=None)
    action: Mapped[str] = mapped_column(String(120), index=True)
    target_type: Mapped[str | None] = mapped_column(String(64), default=None)
    target_id: Mapped[str | None] = mapped_column(String(64), default=None)
    detail: Mapped[dict | None] = mapped_column(JSONB, default=None)
