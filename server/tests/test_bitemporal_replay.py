"""The M2 acceptance test: replay an inventory sequence and prove both time
axes answer correctly — valid time ("what was true at T") and record time
("what did we believe at T").
"""

import asyncio
import uuid
from datetime import UTC, datetime, timedelta

import pytest
from sqlalchemy.exc import IntegrityError

from openrmm.bitemporal.query import get_facts
from openrmm.bitemporal.store import record_facts
from openrmm.db.engine import get_sessionmaker
from openrmm.models.device import Device, OsFamily
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _mk_device() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        device = Device(id=uuid7(), hostname="replay-test", os_family=OsFamily.linux)
        db.add(device)
    return device.id


def _keys(facts: list[dict]) -> dict[str, dict]:
    return {f["fact_key"]: f["payload"] for f in facts}


async def test_replay_valid_and_record_time():
    device_id = await _mk_device()
    t0 = datetime.now(UTC) - timedelta(days=3)
    t1 = datetime.now(UTC) - timedelta(days=2)
    t2 = datetime.now(UTC) - timedelta(days=1)
    sm = get_sessionmaker()

    # T0 observation: openssl 1.1, curl 8.0
    async with sm() as db, db.begin():
        r = await record_facts(
            db,
            "software",
            device_id,
            {"pkg:openssl": {"version": "1.1"}, "pkg:curl": {"version": "8.0"}},
            observed_at=t0,
        )
    assert (r.added, r.changed, r.removed) == (2, 0, 0)
    knew_after_t0 = datetime.now(UTC)
    await asyncio.sleep(0.05)

    # T1 observation: openssl upgraded to 3.0
    async with sm() as db, db.begin():
        r = await record_facts(
            db,
            "software",
            device_id,
            {"pkg:openssl": {"version": "3.0"}, "pkg:curl": {"version": "8.0"}},
            observed_at=t1,
        )
    assert (r.added, r.changed, r.unchanged) == (0, 1, 1)
    await asyncio.sleep(0.05)

    # T2 observation: openssl removed entirely
    async with sm() as db, db.begin():
        r = await record_facts(
            db, "software", device_id, {"pkg:curl": {"version": "8.0"}}, observed_at=t2
        )
    assert (r.removed, r.unchanged) == (1, 1)

    async with sm() as db:
        # Current: only curl remains
        assert set(_keys(await get_facts(db, "software", device_id))) == {"pkg:curl"}

        # Valid-time travel with today's knowledge:
        between_t0_t1 = _keys(
            await get_facts(db, "software", device_id, as_of=t0 + timedelta(hours=1))
        )
        assert between_t0_t1["pkg:openssl"] == {"version": "1.1"}

        between_t1_t2 = _keys(
            await get_facts(db, "software", device_id, as_of=t1 + timedelta(hours=1))
        )
        assert between_t1_t2["pkg:openssl"] == {"version": "3.0"}

        after_t2 = _keys(await get_facts(db, "software", device_id, as_of=t2 + timedelta(hours=1)))
        assert "pkg:openssl" not in after_t2

        # Record-time travel: at knew_after_t0 we believed openssl 1.1 was
        # STILL CURRENT at what later turned out to be its upgrade time.
        believed = _keys(
            await get_facts(
                db,
                "software",
                device_id,
                as_of=t1 + timedelta(hours=1),
                knew_at=knew_after_t0,
            )
        )
        assert believed["pkg:openssl"] == {"version": "1.1"}


async def test_exclusion_constraint_rejects_overlapping_current_beliefs():
    device_id = await _mk_device()
    sm = get_sessionmaker()
    t0 = datetime.now(UTC) - timedelta(days=1)

    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, {"pkg:x": {"version": "1"}}, observed_at=t0)

    # A rogue direct insert of a second open belief for the same fact must be
    # rejected by the database itself, not just by store.py discipline.
    from sqlalchemy.dialects.postgresql import Range

    from openrmm.models.facts import FactSoftware

    with pytest.raises(IntegrityError):
        async with sm() as db, db.begin():
            db.add(
                FactSoftware(
                    device_id=device_id,
                    fact_key="pkg:x",
                    payload={"version": "2"},
                    valid_during=Range(t0, None, bounds="[)"),
                    recorded_during=Range(datetime.now(UTC), None, bounds="[)"),
                )
            )
            await db.flush()


async def test_idempotent_snapshot_writes_nothing():
    device_id = await _mk_device()
    sm = get_sessionmaker()
    snapshot = {"pkg:a": {"version": "1"}, "pkg:b": {"version": "2"}}

    async with sm() as db, db.begin():
        await record_facts(db, "software", device_id, snapshot)
    async with sm() as db, db.begin():
        r = await record_facts(db, "software", device_id, snapshot)
    assert not r.wrote
    assert r.unchanged == 2


async def test_fact_can_come_back_after_removal():
    """A fact that disappears and returns must become visible again.

    Uses `logins` deliberately: everyone logging out and back in is the
    everyday version of vanish-and-return, whereas a host going to zero
    installed packages is not a thing that happens (and `software` is now
    guarded against exactly that, see EMPTY_IS_IMPLAUSIBLE).

    Regression: the reconciliation SELECT filtered only on
    upper_inf(recorded_during), so it also matched CORRECTION rows, which are
    current beliefs with a CLOSED valid range. A returning fact compared equal
    to its own tombstone, was counted "unchanged", and stayed invisible
    forever while the agent kept reporting it.
    """
    device_id = await _mk_device()
    sm = get_sessionmaker()
    snapshot = {"login:rmm@pts/0": {"user": "rmm", "kind": "remote"}}

    async with sm() as db, db.begin():
        await record_facts(db, "logins", device_id, snapshot)
    async with sm() as db:
        assert set(_keys(await get_facts(db, "logins", device_id))) == {"login:rmm@pts/0"}

    # it goes away
    async with sm() as db, db.begin():
        r = await record_facts(db, "logins", device_id, {})
    assert r.removed == 1
    async with sm() as db:
        assert await get_facts(db, "logins", device_id) == []

    # ...and comes back, identical
    async with sm() as db, db.begin():
        r = await record_facts(db, "logins", device_id, snapshot)
    assert r.added == 1, "a returning fact must be recorded again"
    async with sm() as db:
        current = _keys(await get_facts(db, "logins", device_id))
    assert current == snapshot, "the fact must be visible again after returning"


async def test_repeated_change_keeps_exactly_one_current_belief():
    """Two version bumps must not leave overlapping current beliefs.

    After a change there are two rows with an open recorded_during (the
    correction and the successor). If reconciliation picks the correction, the
    next amend writes an overlapping range and the exclusion constraint fires,
    permanently poisoning ingest for that device.
    """
    device_id = await _mk_device()
    sm = get_sessionmaker()
    base = datetime.now(UTC) - timedelta(days=3)

    for i, version in enumerate(["1.0", "2.0", "3.0", "4.0"]):
        async with sm() as db, db.begin():
            await record_facts(
                db,
                "software",
                device_id,
                {"pkg:x": {"version": version}},
                observed_at=base + timedelta(hours=i),
            )

    async with sm() as db:
        assert _keys(await get_facts(db, "software", device_id)) == {"pkg:x": {"version": "4.0"}}
        # every earlier version must still be recoverable on the valid-time axis
        for i, version in enumerate(["1.0", "2.0", "3.0"]):
            at = base + timedelta(hours=i, minutes=30)
            assert _keys(await get_facts(db, "software", device_id, as_of=at)) == {
                "pkg:x": {"version": version}
            }, f"history for {version} was lost"
