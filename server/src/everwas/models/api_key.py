import uuid
from datetime import datetime

from sqlalchemy import ARRAY, DateTime, ForeignKey, String, func
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base
from everwas.models.org import DEFAULT_ORG_ID


class ApiKey(Base):
    """Bearer key `ewpk_<id>_<secret>`; only sha256(secret) is stored."""

    __tablename__ = "api_keys"

    # Tenant boundary. Nullable and unenforced for now: see
    # everwas.models.org. Queries do NOT filter on it yet.
    org_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("organizations.id", ondelete="RESTRICT"),
        default=DEFAULT_ORG_ID,
        index=True,
    )

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120))
    key_id: Mapped[str] = mapped_column(String(32), unique=True, index=True)
    secret_hash: Mapped[str] = mapped_column(String(64))
    scopes: Mapped[list[str]] = mapped_column(ARRAY(String), default=list)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    expires_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    last_used_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
