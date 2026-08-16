import enum
import uuid
from datetime import datetime

from sqlalchemy import DateTime, Enum, ForeignKey, String, func

# postgresql.ARRAY, not the generic one: the generic comparator has no
# overlap()/contains(), so tag targeting silently fails to compile.
from sqlalchemy.dialects.postgresql import ARRAY
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class OsFamily(enum.StrEnum):
    windows = "windows"
    macos = "macos"
    linux = "linux"


class DeviceStatus(enum.StrEnum):
    enrolled = "enrolled"  # enrolled, never heartbeated
    active = "active"
    offline = "offline"
    retired = "retired"


class Site(Base):
    __tablename__ = "sites"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class Device(Base):
    __tablename__ = "devices"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True)  # uuid7, is the agent_id
    site_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("sites.id", ondelete="SET NULL"), default=None
    )
    hostname: Mapped[str] = mapped_column(String(255), index=True)
    os_family: Mapped[OsFamily] = mapped_column(Enum(OsFamily, name="os_family"))
    os_version: Mapped[str] = mapped_column(String(120), default="")
    arch: Mapped[str] = mapped_column(String(32), default="")
    agent_version: Mapped[str] = mapped_column(String(64), default="")
    status: Mapped[DeviceStatus] = mapped_column(
        Enum(DeviceStatus, name="device_status"), default=DeviceStatus.enrolled, index=True
    )
    tags: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    last_heartbeat_at: Mapped[datetime | None] = mapped_column(
        DateTime(timezone=True), default=None
    )
    enrolled_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now()
    )


class EnrollmentToken(Base):
    __tablename__ = "enrollment_tokens"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    token_hash: Mapped[str] = mapped_column(String(64), unique=True)
    site_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("sites.id", ondelete="CASCADE"), default=None
    )
    max_uses: Mapped[int] = mapped_column(default=1)
    uses: Mapped[int] = mapped_column(default=0)
    expires_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    created_by: Mapped[str | None] = mapped_column(String(255), default=None)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class AgentCredential(Base):
    __tablename__ = "agent_credentials"

    device_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("devices.id", ondelete="CASCADE"), primary_key=True
    )
    secret_hash: Mapped[str] = mapped_column(String(64))
    rotated_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
