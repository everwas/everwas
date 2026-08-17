"""Bitemporal fact tables.

Every fact carries two time axes:
- valid_during: when it was true on the machine (wire time)
- recorded_during: when we believed it (belief time); open upper bound = current

Never UPDATE payloads. All writes go through openrmm.bitemporal.store (the
sequenced-amend pattern); the GiST exclusion constraints in migration 0003
reject overlapping current beliefs as a safety net.
"""

import uuid
from datetime import datetime
from typing import Literal, get_args

from sqlalchemy import BigInteger, ForeignKey, String, Text
from sqlalchemy.dialects.postgresql import JSONB, TSTZRANGE, Range
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class BitemporalFactMixin:
    id: Mapped[int] = mapped_column(BigInteger, primary_key=True, autoincrement=True)
    fact_key: Mapped[str] = mapped_column(Text)
    payload: Mapped[dict] = mapped_column(JSONB)
    valid_during: Mapped[Range[datetime]] = mapped_column(TSTZRANGE)
    recorded_during: Mapped[Range[datetime]] = mapped_column(TSTZRANGE)
    source: Mapped[str] = mapped_column(String(32), default="agent")


class FactHardware(BitemporalFactMixin, Base):
    __tablename__ = "fact_hardware"
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))


class FactSoftware(BitemporalFactMixin, Base):
    __tablename__ = "fact_software"
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))


class FactPatchState(BitemporalFactMixin, Base):
    __tablename__ = "fact_patch_state"
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))


class FactNetwork(BitemporalFactMixin, Base):
    __tablename__ = "fact_network"
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))


class FactLogins(BitemporalFactMixin, Base):
    __tablename__ = "fact_logins"
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))


FACT_TABLES = {
    "hardware": FactHardware,
    "software": FactSoftware,
    "patchstate": FactPatchState,
    "network": FactNetwork,
    "logins": FactLogins,
}

# The API validates its ?kind= against this. A Literal cannot be built from a
# dict at type-check time, so the assertion below keeps the two in step instead
# of trusting them to stay that way.
#
# They already drifted once: adding "network" and "logins" to FACT_TABLES left
# the endpoint's own hardcoded Literal behind, and the only symptom was a 422 in
# the browser for a kind the server stores perfectly well. Failing at import is
# a better place to find that out.
FactKind = Literal["hardware", "software", "patchstate", "network", "logins"]

assert set(get_args(FactKind)) == set(FACT_TABLES), (
    f"FactKind and FACT_TABLES have drifted: {set(get_args(FactKind)) ^ set(FACT_TABLES)}"
)
