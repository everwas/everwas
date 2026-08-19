"""A snapshot that arrives late must not rewrite valid-time history.

`record_facts` compared an incoming snapshot only against current beliefs and
never asked whether its `observed_at` predates the belief it was about to
supersede. An agent whose clock was corrected, or that flushed a spool after
being offline, could therefore overwrite a newer truth with an older one, and
the newer value vanished from the valid-time axis entirely.

That is the axis every incident question uses: as_of is "what was true on the
machine at 03:00", and the answer became the value from 04:00 that arrived
afterwards. The older value is not marked as late or suspect; it simply becomes
the answer.
"""

import uuid
from datetime import UTC, datetime, timedelta

import pytest

from everwas.bitemporal.query import get_facts
from everwas.bitemporal.store import StaleObservationError, record_facts
from everwas.db.engine import get_sessionmaker
from everwas.models.device import Device, OsFamily
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

T0 = datetime(2026, 8, 17, 4, 0, tzinfo=UTC)
T1 = datetime(2026, 8, 17, 8, 0, tzinfo=UTC)


async def _device() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        d = Device(id=uuid7(), hostname="late-host", os_family=OsFamily.linux, tags=[])
        db.add(d)
        await db.flush()
        return d.id


async def test_a_late_snapshot_does_not_overwrite_a_newer_belief():
    device_id = await _device()
    sm = get_sessionmaker()

    # 08:00: the truth we have.
    async with sm() as db, db.begin():
        await record_facts(db, "hardware", device_id, {"os": {"version": "24.04"}}, observed_at=T1)

    # 04:00 arrives afterwards: a spooled snapshot from a host whose clock was
    # wrong, or that was offline. Well inside MAX_LAG, so wire-time validation
    # passes it through.
    async with sm() as db, db.begin():
        with pytest.raises(StaleObservationError):
            await record_facts(
                db, "hardware", device_id, {"os": {"version": "22.04"}}, observed_at=T0
            )

    async with sm() as db:
        # The 08:00 truth survives, both as current and at its own instant.
        current = await get_facts(db, "hardware", device_id)
        assert current[0]["payload"] == {"version": "24.04"}
        at_0830 = await get_facts(db, "hardware", device_id, as_of=T1 + timedelta(minutes=30))
        assert at_0830[0]["payload"] == {"version": "24.04"}


async def test_a_late_snapshot_cannot_poison_ingest_via_the_exclusion_constraint():
    """The second, worse shape: a permanent poison message.

    Where the fact already changed, a correction row covers [T0, T1). A snapshot
    landing inside that window inserts an overlapping range, the GiST exclusion
    fires, and the message is retried to exhaustion and dead-lettered. The whole
    snapshot is discarded, not just the conflicting key.
    """
    device_id = await _device()
    sm = get_sessionmaker()
    t_mid = T0 + timedelta(hours=2)

    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, {"pkg:ssl": {"version": "1"}}, observed_at=T0)
    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, {"pkg:ssl": {"version": "3"}}, observed_at=T1)

    # Lands between the two, inside the correction row's window.
    async with sm() as db, db.begin():
        with pytest.raises(StaleObservationError):
            await record_facts(
                db, "software", device_id, {"pkg:ssl": {"version": "2"}}, observed_at=t_mid
            )


async def test_an_observation_at_the_same_instant_is_still_accepted():
    # Equal timestamps are ordinary: a re-publish of the same snapshot, or two
    # kinds collected in the same cycle. Only strictly-older is refused.
    device_id = await _device()
    sm = get_sessionmaker()
    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, {"pkg:a": {"version": "1"}}, observed_at=T1)
    async with sm() as db, db.begin():
        res = await record_facts(
            db, "software", device_id, {"pkg:a": {"version": "2"}}, observed_at=T1
        )
    assert res.changed == 1


async def test_a_newer_observation_is_accepted_normally():
    device_id = await _device()
    sm = get_sessionmaker()
    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, {"pkg:a": {"version": "1"}}, observed_at=T0)
    async with sm() as db, db.begin():
        res = await record_facts(
            db, "software", device_id, {"pkg:a": {"version": "2"}}, observed_at=T1
        )
    assert res.changed == 1
