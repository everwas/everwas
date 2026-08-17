"""A snapshot that retires an entire kind is refused.

The agent is now careful to never publish an empty set it did not verify, but
the server must not depend on that. `record_facts` treats a snapshot as the
complete truth for its kind, so a single `{}` payload retires every fact the
device has: every installed package, every pending patch, every interface.

That is one buggy publisher, one truncated JSON body, or one collector added
later without the rule, away from erasing a device's inventory. And it is
plausible-looking when it happens, because the tombstones are legitimate
bitemporal history: an `as_of` query afterwards agrees that the packages ended.
"""

import uuid

import pytest

from openrmm.bitemporal.store import WholesaleRetirementError, record_facts
from openrmm.db.engine import get_sessionmaker
from openrmm.models.device import Device, OsFamily
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

WHEN = __import__("datetime").datetime(2026, 8, 17, 12, tzinfo=__import__("datetime").UTC)


async def _device():
    async with get_sessionmaker()() as db, db.begin():
        d = Device(id=uuid7(), hostname="retire-host", os_family=OsFamily.linux, tags=[])
        db.add(d)
        await db.flush()
        return d.id


async def test_an_empty_snapshot_cannot_retire_every_fact():
    device_id = await _device()
    packages = {f"pkg:{n}": {"version": "1"} for n in ("bash", "curl", "openssl")}

    async with get_sessionmaker()() as db, db.begin():
        await record_facts(db, "software", device_id, packages, observed_at=WHEN)

    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(WholesaleRetirementError) as caught:
            await record_facts(db, "software", device_id, {}, observed_at=WHEN)
    # The message has to name the device and the count, because the operator
    # reading it needs to know which host to go and look at.
    assert "software" in str(caught.value)
    assert "3" in str(caught.value)

    # Nothing was retired.
    async with get_sessionmaker()() as db:
        from openrmm.bitemporal.query import get_facts

        still = await get_facts(db, "software", device_id)
        assert len(still) == 3


async def test_removing_some_facts_is_still_allowed():
    device_id = await _device()
    async with get_sessionmaker()() as db, db.begin():
        await record_facts(
            db,
            "software",
            device_id,
            {"pkg:bash": {"version": "1"}, "pkg:curl": {"version": "1"}},
            observed_at=WHEN,
        )

    # Uninstalling one package is ordinary and must not trip the guard.
    async with get_sessionmaker()() as db, db.begin():
        res = await record_facts(
            db, "software", device_id, {"pkg:bash": {"version": "1"}}, observed_at=WHEN
        )
    assert res.removed == 1


async def test_a_device_with_no_facts_yet_is_not_a_retirement():
    # First contact legitimately sends nothing for a kind the host has none of
    # (a container with no patch backend). Zero of zero is not a wipe.
    device_id = await _device()
    async with get_sessionmaker()() as db, db.begin():
        res = await record_facts(db, "patchstate", device_id, {}, observed_at=WHEN)
    assert res.removed == 0
