"""Issued device certificates.

The private key is never here and never anywhere on the server: it is generated
on the endpoint, ideally in a TPM, and only the CSR crosses the wire. What is
stored is the certificate, so the fleet can be listed and monitored, and enough
to revoke it by serial.
"""

import uuid
from datetime import datetime

from sqlalchemy import DateTime, ForeignKey, String, Text, func
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class DeviceCertificate(Base):
    __tablename__ = "device_certificates"

    #: The X.509 serial, as a hex string. A 20-byte integer does not fit in
    #: bigint, and this is the value a CRL revokes by.
    serial: Mapped[str] = mapped_column(String(64), primary_key=True)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    common_name: Mapped[str] = mapped_column(Text)
    certificate_pem: Mapped[str] = mapped_column(Text)
    fingerprint_sha256: Mapped[str] = mapped_column(String(64))
    not_before: Mapped[datetime] = mapped_column(DateTime(timezone=True))
    not_after: Mapped[datetime] = mapped_column(DateTime(timezone=True))
    issued_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    revoked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True), default=None)
    revocation_reason: Mapped[str | None] = mapped_column(String(64), default=None)
