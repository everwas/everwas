"""Device lifecycle beyond enrollment: permanent removal.

Retiring a device revokes it but keeps the row, which is right: the alerts,
script runs and audit trail that reference it are the record of what that
machine did, and they should survive the machine. But nothing could ever
remove one, so decommissioned hardware and load-test fixtures accumulated in
every list and every device picker for good.

Deletion is therefore a SEPARATE, second step, allowed only on a device that
is already retired. Two deliberate actions to destroy history, and no way to
delete a machine that is still running.
"""

import uuid

import structlog
from sqlalchemy import delete, select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.telemetry import PARTITIONED_TELEMETRY

log = structlog.get_logger()


class DeviceNotRetiredError(Exception):
    """Deletion was attempted on a device that has not been retired."""


async def delete_device(db: AsyncSession, device_id: uuid.UUID, actor: str) -> Device | None:
    """Remove a retired device and everything that hangs off it.

    Returns the deleted device, or None if it does not exist. Raises
    DeviceNotRetiredError if it is still live.
    """
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None:
        return None
    if device.status is not DeviceStatus.retired:
        raise DeviceNotRetiredError(
            f"{device.hostname} is {device.status.value}; retire it first. Deleting a live "
            "device would drop its history while the agent keeps reporting."
        )

    # The telemetry tables are partitioned time-series with device_id as a
    # plain column and NO foreign key, so the cascade that cleans up every
    # other table does not touch them. Without this they sit orphaned in the
    # partitions until retention drops them, which is weeks of rows belonging
    # to a device that no longer exists.
    # Every partitioned telemetry table, derived from the maintenance list
    # rather than written out again. This loop already fell behind once:
    # telemetry_network was added in migration 0013 and not added here, so a
    # deleted device kept its per-interface counters for the retention window.
    for table in PARTITIONED_TELEMETRY:
        await db.execute(delete(table).where(table.c.device_id == device_id))

    # Written BEFORE the delete: audit_log has no FK to devices (it outlives
    # its subjects on purpose), but the hostname does not survive the row.
    db.add(
        AuditLog(
            # Read off the device while it still exists. This is the entry the
            # whole write-before-delete dance is for, so it must be readable
            # after the row it describes is gone.
            org_id=device.org_id,
            actor_type=ActorType.user,
            actor_id=actor,
            action="device.deleted",
            target_type="device",
            target_id=str(device_id),
            detail={"hostname": device.hostname, "os_family": device.os_family.value},
        )
    )
    await db.flush()

    # Everything else (facts, alerts, runs, patch jobs, snapshots, sessions)
    # has ON DELETE CASCADE and goes with it.
    await db.delete(device)
    await db.flush()
    log.info("device deleted", device_id=str(device_id), hostname=device.hostname, actor=actor)
    return device
