"""Permanent device removal.

Retiring revokes a device but keeps the row, which is right: its alerts, runs
and audit trail are the record of what that machine did. But nothing could
ever remove one, so decommissioned hardware and load-test fixtures piled up in
every list and picker for good.

These pin the two properties that make deletion safe to expose: it cannot
touch a live device, and it does not leave orphans behind.
"""

import uuid
from datetime import UTC, datetime

import pytest
from sqlalchemy import func, insert, select

from everwas.db.engine import get_sessionmaker
from everwas.models.audit import AuditLog
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.models.telemetry import telemetry_metrics
from everwas.services.devices import DeviceNotRetiredError, delete_device
from everwas.services.enrollment import retire_device
from everwas.services.partitions import ensure_partitions
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _device(status: DeviceStatus = DeviceStatus.active) -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        d = Device(id=uuid7(), hostname="doomed", os_family=OsFamily.linux, status=status)
        db.add(d)
    return d.id


async def _telemetry_rows(device_id: uuid.UUID) -> int:
    async with get_sessionmaker()() as db:
        return (
            await db.execute(
                select(func.count())
                .select_from(telemetry_metrics)
                .where(telemetry_metrics.c.device_id == device_id)
            )
        ).scalar_one()


async def test_a_live_device_cannot_be_deleted():
    """The guard that makes this safe to put behind a button. Deleting a
    running machine drops its history while the agent goes on reporting into
    a device row that no longer exists."""
    device_id = await _device(DeviceStatus.active)

    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(DeviceNotRetiredError):
            await delete_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        assert await db.get(Device, device_id) is not None


async def test_retire_then_delete_works():
    device_id = await _device()

    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")
    async with get_sessionmaker()() as db, db.begin():
        assert await delete_device(db, device_id, actor="admin@example.com") is not None

    async with get_sessionmaker()() as db:
        assert await db.get(Device, device_id) is None


async def test_deleting_an_unknown_device_is_not_an_error():
    async with get_sessionmaker()() as db, db.begin():
        assert await delete_device(db, uuid7(), actor="admin@example.com") is None


async def test_telemetry_goes_too():
    """The partitioned telemetry tables have device_id as a plain column and
    NO foreign key, so the cascade that cleans up everything else misses them
    entirely. Without an explicit delete they sit orphaned in the partitions
    for the whole retention window."""
    device_id = await _device()
    async with get_sessionmaker()() as db, db.begin():
        # Partitions are created by the maintenance job, not the migration, so
        # a scratch database has none and the insert would fail on the range.
        await ensure_partitions(db, retention_days=30)
        await db.execute(
            insert(telemetry_metrics).values(
                device_id=device_id, ts=datetime.now(UTC), cpu_pct=12.5
            )
        )
    assert await _telemetry_rows(device_id) == 1

    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")
    async with get_sessionmaker()() as db, db.begin():
        await delete_device(db, device_id, actor="admin@example.com")

    assert await _telemetry_rows(device_id) == 0, "telemetry outlived the device it belongs to"


async def test_deletion_is_audited_with_the_hostname():
    """audit_log has no FK to devices on purpose, so it outlives them. But the
    hostname does not survive the row, so it has to be captured before the
    delete or the trail says only that some uuid was removed."""
    device_id = await _device()
    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, device_id, actor="admin@example.com")
    async with get_sessionmaker()() as db, db.begin():
        await delete_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        row = (
            await db.execute(select(AuditLog).where(AuditLog.action == "device.deleted"))
        ).scalar_one()
        assert row.target_id == str(device_id)
        assert row.detail["hostname"] == "doomed"


async def test_retired_devices_are_out_of_the_default_list():
    """A device picker padded with machines that will never report again is
    how the ones that matter get lost."""
    live = await _device(DeviceStatus.active)
    gone = await _device()
    async with get_sessionmaker()() as db, db.begin():
        await retire_device(db, gone, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        default = (
            (await db.execute(select(Device).where(Device.status != DeviceStatus.retired)))
            .scalars()
            .all()
        )
        every = (await db.execute(select(Device))).scalars().all()

    assert {d.id for d in default} == {live}
    assert {d.id for d in every} == {live, gone}


async def test_deletion_covers_every_partitioned_telemetry_table():
    """Asserted against the list, not against the tables that existed today.

    This loop already fell behind once: telemetry_network arrived in migration
    0013 and was added to partition maintenance but not to deletion, so a
    deleted device kept its per-interface counters for the whole retention
    window. Naming the tables again here would have the same failure mode as
    the code did, so this walks the shared list instead and will fail the day a
    new partitioned table is added without being wired into deletion.
    """
    from sqlalchemy import func, insert, select

    from everwas.models.telemetry import PARTITIONED_TELEMETRY

    device_id = await _device(DeviceStatus.retired)
    ts = datetime.now(UTC)

    async with get_sessionmaker()() as db, db.begin():
        await ensure_partitions(db, retention_days=30)
        for table in PARTITIONED_TELEMETRY:
            row = {"device_id": device_id, "ts": ts}
            # Fill whatever extra key columns the table has, so the insert is
            # valid for every shape without naming them one by one.
            for col in table.primary_key.columns:
                if col.name not in row:
                    row[col.name] = "x"
            await db.execute(insert(table).values(row))

    async with get_sessionmaker()() as db, db.begin():
        await delete_device(db, device_id, actor="admin@example.com")

    async with get_sessionmaker()() as db:
        for table in PARTITIONED_TELEMETRY:
            left = (
                await db.execute(
                    select(func.count()).select_from(table).where(table.c.device_id == device_id)
                )
            ).scalar_one()
            assert left == 0, f"{table.name} kept rows for a deleted device"
