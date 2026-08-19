import enum
import uuid
from datetime import datetime

from sqlalchemy import DateTime, Enum, ForeignKey, String, Text, func

# postgresql.ARRAY, not the generic one: the generic comparator has no
# overlap()/contains(), so tag targeting silently fails to compile.
from sqlalchemy.dialects.postgresql import ARRAY
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base
from everwas.models.org import DEFAULT_ORG_ID


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

    # Tenant boundary. Nullable and unenforced for now: see
    # everwas.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    description: Mapped[str | None] = mapped_column(Text)
    address: Mapped[str | None] = mapped_column(Text)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())


class Device(Base):
    __tablename__ = "devices"

    # Tenant boundary. Nullable and unenforced for now: see
    # everwas.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

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
    #: The 802.1X certificate this device says it is ACTUALLY holding, as
    #: reported in its heartbeat. Not the same fact as the newest row in
    #: device_certificates, which is what we last issued it: they diverge on a
    #: renewal that half-failed, on a machine restored from a backup image, and
    #: on material deleted by hand. None means the agent has not told us, which
    #: is the honest state for an old agent and for any fleet not using 802.1X.
    reported_cert_serial: Mapped[str | None] = mapped_column(String(64), default=None, index=True)
    reported_cert_not_after: Mapped[datetime | None] = mapped_column(
        DateTime(timezone=True), default=None
    )
    #: When it last told us. Separates "reported nothing just now", meaning the
    #: material is genuinely gone, from "never reported", meaning we do not know.
    reported_cert_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    enrolled_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now()
    )


class EnrollmentToken(Base):
    __tablename__ = "enrollment_tokens"

    # Tenant boundary. Nullable and unenforced for now: see
    # everwas.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

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

    # The secret being replaced, honoured until prev_valid_until. Rotation is
    # a distributed handshake that can be interrupted at any point, so both
    # secrets work during the window and whichever one the agent ends up
    # holding gets it back in.
    prev_secret_hash: Mapped[str | None] = mapped_column(String(64), default=None)
    prev_valid_until: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
