"""Machine identity in the hardware snapshot.

The identity fact is conditional: an agent that reported no DMI fields never
records one, so the server holds no belief rather than a false "this machine
has no serial". An upgraded agent then ADDS the fact; a changed serial amends
it and both values remain reachable on the valid-time axis.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest

from everwas.bitemporal.query import get_facts
from everwas.db.engine import get_sessionmaker, session_scope
from everwas.ingest.inventory import apply_inventory
from everwas.models.device import Device, OsFamily
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

OLD_AGENT_HW = {
    "cpu_model": "AMD Ryzen 7",
    "cpu_cores": 16,
    "mem_total": 68719476736,
    "hostname": "web-01",
    "os_family": "linux",
    "os_version": "Arch Linux",
    "kernel": "6.9.1",
    "arch": "x86_64",
    "virtualization": "",
}

DMI_FIELDS = {
    "manufacturer": "LENOVO",
    "model": "21CB000JUS",
    "serial_number": "PF3K2ABC",
    "chassis_type": "laptop",
}


async def _mk_device() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        device = Device(id=uuid7(), hostname="web-01", os_family=OsFamily.linux)
        db.add(device)
    return device.id


async def _hardware_facts(device_id: uuid.UUID, **kwargs) -> dict[str, dict]:
    async with session_scope() as db:
        facts = await get_facts(db, "hardware", device_id, **kwargs)
    return {f["fact_key"]: f["payload"] for f in facts}


async def test_old_agent_records_no_identity_fact():
    device_id = await _mk_device()
    async with session_scope() as db:
        await apply_inventory(db, device_id, "hardware", datetime.now(UTC), dict(OLD_AGENT_HW))

    facts = await _hardware_facts(device_id)
    assert set(facts) == {"cpu", "memory", "os", "system"}


async def test_dmi_agent_records_identity():
    device_id = await _mk_device()
    async with session_scope() as db:
        await apply_inventory(
            db, device_id, "hardware", datetime.now(UTC), OLD_AGENT_HW | DMI_FIELDS
        )

    facts = await _hardware_facts(device_id)
    assert facts["identity"] == {
        "manufacturer": "LENOVO",
        "model": "21CB000JUS",
        "serial_number": "PF3K2ABC",
        "chassis_type": "laptop",
    }


async def test_partial_identity_still_recorded():
    """One real field is enough — chassis without a readable serial is
    common on VMs and still worth believing."""
    device_id = await _mk_device()
    async with session_scope() as db:
        await apply_inventory(
            db,
            device_id,
            "hardware",
            datetime.now(UTC),
            OLD_AGENT_HW | {"chassis_type": "desktop"},
        )

    facts = await _hardware_facts(device_id)
    assert facts["identity"]["chassis_type"] == "desktop"
    assert facts["identity"]["serial_number"] == ""


async def test_changed_serial_amends_and_history_survives():
    device_id = await _mk_device()
    t0 = datetime.now(UTC) - timedelta(days=2)
    t1 = datetime.now(UTC) - timedelta(days=1)

    async with session_scope() as db:
        await apply_inventory(db, device_id, "hardware", t0, OLD_AGENT_HW | DMI_FIELDS)
    async with session_scope() as db:
        await apply_inventory(
            db,
            device_id,
            "hardware",
            t1,
            OLD_AGENT_HW | DMI_FIELDS | {"serial_number": "REPLACED9"},
        )

    now_facts = await _hardware_facts(device_id)
    assert now_facts["identity"]["serial_number"] == "REPLACED9"

    then_facts = await _hardware_facts(device_id, as_of=t0 + timedelta(hours=1))
    assert then_facts["identity"]["serial_number"] == "PF3K2ABC"
