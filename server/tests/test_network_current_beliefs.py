"""The device network view must show the CURRENT address, deterministically.

A bitemporal fact that changes leaves two rows with an open recorded_during:
the successor, and a correction describing the value that ended. Both are
current beliefs. Only one is a current fact.

Filtering on recorded_during alone therefore returns both, and a dict built
from them keeps whichever the database happened to return last. The store
learned this once already; the network endpoint reintroduced it.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest

from everwas.bitemporal.store import record_facts
from everwas.db.engine import get_sessionmaker
from everwas.models.device import Device, OsFamily
from everwas.models.user import Role
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

T0 = datetime(2026, 8, 17, 6, 0, tzinfo=UTC)
T1 = T0 + timedelta(hours=1)


async def _device_with_moved_interface() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        d = Device(id=uuid7(), hostname="net-move", os_family=OsFamily.linux, tags=[])
        db.add(d)
        await db.flush()
        device_id = d.id

    sm = get_sessionmaker()
    async with sm() as db, db.begin():
        await record_facts(
            db,
            "network",
            device_id,
            {"iface:eth0": {"name": "eth0", "up": True, "addresses": ["10.0.1.5/24"]}},
            observed_at=T0,
        )
    # The address changes, which creates the correction row.
    async with sm() as db, db.begin():
        await record_facts(
            db,
            "network",
            device_id,
            {"iface:eth0": {"name": "eth0", "up": True, "addresses": ["10.0.1.9/24"]}},
            observed_at=T1,
        )
    return device_id


async def test_the_endpoints_filter_selects_exactly_one_row_per_interface():
    """Asserted on the PREDICATE, not on the value it happened to return.

    Checking that the endpoint reports the new address passes with the bug
    present roughly half the time, because which of the two matching rows wins
    is physical row order. A test that passes by luck is worse than no test, so
    this counts the rows the filter matches: with recorded_during alone it is
    two, and the second one is the correction describing the address that
    ended.
    """
    from sqlalchemy import func, select

    from everwas.models.facts import FactNetwork

    device_id = await _device_with_moved_interface()

    async with get_sessionmaker()() as db:
        belief_only = (
            await db.execute(
                select(func.count())
                .select_from(FactNetwork)
                .where(
                    FactNetwork.device_id == device_id,
                    func.upper_inf(FactNetwork.recorded_during),
                )
            )
        ).scalar_one()
        # The premise: an amend really does leave two open beliefs. If this is
        # ever 1, the correction-row behaviour changed and the guard below is
        # measuring nothing.
        assert belief_only == 2, "expected a correction row alongside the successor"

        current_fact = (
            await db.execute(
                select(func.count())
                .select_from(FactNetwork)
                .where(
                    FactNetwork.device_id == device_id,
                    func.upper_inf(FactNetwork.recorded_during),
                    func.upper_inf(FactNetwork.valid_during),
                )
            )
        ).scalar_one()
        assert current_fact == 1, "a current FACT is one row; a current BELIEF is not"


async def test_the_network_view_reports_the_current_address(client_as):
    device_id = await _device_with_moved_interface()
    async with client_as(Role.viewer) as c:
        r = await c.get(f"/api/v1/devices/{device_id}/network")
    assert r.status_code == 200
    ifaces = {i["name"]: i for i in r.json()}
    assert ifaces["eth0"]["addresses"] == ["10.0.1.9/24"]
    assert len(r.json()) == 1
