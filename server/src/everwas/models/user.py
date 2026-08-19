import enum
import uuid
from datetime import datetime

from sqlalchemy import DateTime, Enum, ForeignKey, String, func
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base
from everwas.models.org import DEFAULT_ORG_ID


class Role(enum.StrEnum):
    admin = "admin"
    operator = "operator"
    viewer = "viewer"


class User(Base):
    __tablename__ = "users"

    # Tenant boundary. Nullable and unenforced for now: see
    # everwas.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    email: Mapped[str] = mapped_column(String(255), unique=True, index=True)
    password_hash: Mapped[str] = mapped_column(String(255))
    role: Mapped[Role] = mapped_column(Enum(Role, name="user_role"), default=Role.viewer)
    totp_secret: Mapped[str | None] = mapped_column(String(64), default=None)
    is_active: Mapped[bool] = mapped_column(default=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
