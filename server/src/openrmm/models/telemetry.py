"""Telemetry tables (partitioned by day) + hot cache.

The partitioned parents are created in migration 0003 with raw SQL (Alembic
autogenerate can't express PARTITION BY). These Core Table objects exist for
typed inserts/selects only — keep them in sync with the migration.
"""

import uuid
from datetime import datetime

from sqlalchemy import (
    BigInteger,
    Column,
    DateTime,
    Float,
    ForeignKey,
    String,
    Table,
    Text,
    func,
)
from sqlalchemy.dialects.postgresql import JSONB, UUID
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base

telemetry_metrics = Table(
    "telemetry_metrics",
    Base.metadata,
    Column("device_id", UUID(as_uuid=True), primary_key=True),
    Column("ts", DateTime(timezone=True), primary_key=True),
    Column("cpu_pct", Float),
    Column("mem_used", BigInteger),
    Column("mem_total", BigInteger),
    Column("swap_pct", Float),
    Column("load1", Float),
    Column("uptime_s", BigInteger),
    info={"skip_autogenerate": True},
)

telemetry_disks = Table(
    "telemetry_disks",
    Base.metadata,
    Column("device_id", UUID(as_uuid=True), primary_key=True),
    Column("ts", DateTime(timezone=True), primary_key=True),
    Column("mount", Text, primary_key=True),
    Column("used", BigInteger),
    Column("total", BigInteger),
    Column("fstype", Text),
    info={"skip_autogenerate": True},
)


telemetry_network = Table(
    "telemetry_network",
    Base.metadata,
    Column("device_id", UUID(as_uuid=True), primary_key=True),
    Column("ts", DateTime(timezone=True), primary_key=True),
    Column("iface", Text, primary_key=True),
    # Cumulative since boot, stored raw. These are counters, not rates: see
    # migration 0013 for why the derivation is left to query time.
    Column("bytes_sent", BigInteger),
    Column("bytes_recv", BigInteger),
    Column("packets_sent", BigInteger),
    Column("packets_recv", BigInteger),
    Column("err_in", BigInteger),
    Column("err_out", BigInteger),
    Column("drop_in", BigInteger),
    Column("drop_out", BigInteger),
    info={"skip_autogenerate": True},
)


class DeviceStatusLatest(Base):
    """One row per device, upserted on every telemetry sample. The fleet
    dashboard reads only this table — never the partitioned history."""

    __tablename__ = "device_status_latest"

    device_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("devices.id", ondelete="CASCADE"), primary_key=True
    )
    ts: Mapped[datetime] = mapped_column(DateTime(timezone=True))
    cpu_pct: Mapped[float | None] = mapped_column(Float, default=None)
    mem_pct: Mapped[float | None] = mapped_column(Float, default=None)
    worst_disk_pct: Mapped[float | None] = mapped_column(Float, default=None)


class DeviceSnapshot(Base):
    """Latest-only snapshots for churny inventory kinds (processes, services)."""

    __tablename__ = "device_snapshots"

    device_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("devices.id", ondelete="CASCADE"), primary_key=True
    )
    kind: Mapped[str] = mapped_column(String(32), primary_key=True)
    payload: Mapped[dict] = mapped_column(JSONB)
    snapshot_hash: Mapped[str] = mapped_column(String(64), default="")
    updated_at: Mapped[datetime] = mapped_column(
        DateTime(timezone=True), server_default=func.now(), onupdate=func.now()
    )
