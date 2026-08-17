"""Network rate derivation, especially the two cases that produce nonsense.

A naive delta is right most of the time, which is exactly why the wrong
answers here reach production: they only appear after a reboot or an outage,
by which point nobody is looking at the chart that lied.
"""

from datetime import UTC, datetime, timedelta

import pytest
from sqlalchemy import insert

from openrmm.db.engine import get_sessionmaker
from openrmm.ingest.telemetry import apply_telemetry
from openrmm.models.device import Device, OsFamily
from openrmm.models.telemetry import telemetry_network
from openrmm.services.network_telemetry import MAX_GAP_S, interface_rates
from openrmm.services.partitions import ensure_partitions
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

# Anchored to today, not a literal date. Telemetry lives in daily partitions
# that the maintenance job only creates around the present, so a hardcoded date
# passes on the day it is written and fails silently forever after.
BASE = datetime.now(UTC).replace(hour=12, minute=0, second=0, microsecond=0)


@pytest.fixture
async def device_id():
    async with get_sessionmaker()() as db, db.begin():
        # Partitioned tables reject any row with no partition to land in, so
        # this has to happen before the first insert.
        await ensure_partitions(db, retention_days=30)
        device = Device(id=uuid7(), hostname="net-host", os_family=OsFamily.linux, tags=[])
        db.add(device)
        await db.flush()
        return device.id


async def _sample(db, device_id, *, at, iface="eth0", sent=0, recv=0):
    await db.execute(
        insert(telemetry_network).values(
            device_id=device_id, ts=at, iface=iface, bytes_sent=sent, bytes_recv=recv
        )
    )


async def test_a_steady_counter_becomes_a_flat_rate(device_id):
    async with get_sessionmaker()() as db, db.begin():
        for i in range(3):
            # 6000 bytes per 60s = 100 B/s.
            await _sample(db, device_id, at=BASE + timedelta(seconds=60 * i), sent=6000 * i)

        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))

        # Two rates from three samples: the first has nothing to subtract from.
        assert [p["bytes_sent"] for p in series["eth0"]] == [100.0, 100.0]


async def test_a_counter_reset_is_a_gap_not_a_spike(device_id):
    async with get_sessionmaker()() as db, db.begin():
        # The reboot case: the counter goes backwards.
        await _sample(db, device_id, at=BASE, sent=5_000_000_000)
        await _sample(db, device_id, at=BASE + timedelta(seconds=60), sent=1000)
        await _sample(db, device_id, at=BASE + timedelta(seconds=120), sent=61000)

        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))
        rates = [p["bytes_sent"] for p in series["eth0"]]

        # The reset point is unknown, NOT zero and NOT the absolute difference.
        # Guessing a wrap width here is what invents an 83 MB/s spike out of a
        # reboot, and one such point rescales the whole chart.
        assert rates[0] is None
        # The series recovers immediately afterwards.
        assert rates[1] == pytest.approx(1000.0)


async def test_an_outage_is_a_gap_not_a_smeared_average(device_id):
    async with get_sessionmaker()() as db, db.begin():
        await _sample(db, device_id, at=BASE, sent=0)
        # Agent away for an hour, then returns having moved a lot of traffic.
        away = timedelta(seconds=MAX_GAP_S + 60)
        await _sample(db, device_id, at=BASE + away, sent=3_600_000_000)
        await _sample(db, device_id, at=BASE + away + timedelta(seconds=60), sent=3_600_006_000)

        series = await interface_rates(db, device_id, BASE - timedelta(hours=2))
        rates = [p["bytes_sent"] for p in series["eth0"]]

        # Dividing that delta by the elapsed time is arithmetically correct and
        # tells you nothing about any moment that actually happened.
        assert rates[0] is None
        assert rates[1] == pytest.approx(100.0)


async def test_interfaces_do_not_bleed_into_each_other(device_id):
    async with get_sessionmaker()() as db, db.begin():
        # Interleaved in time, so a window that forgot to PARTITION BY iface would
        # subtract eth0 from wlan0 and produce garbage on both.
        await _sample(db, device_id, at=BASE, iface="eth0", sent=0)
        await _sample(db, device_id, at=BASE + timedelta(seconds=30), iface="wlan0", sent=1_000_000)
        await _sample(db, device_id, at=BASE + timedelta(seconds=60), iface="eth0", sent=6000)
        await _sample(db, device_id, at=BASE + timedelta(seconds=90), iface="wlan0", sent=1_000_600)

        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))

        assert [p["bytes_sent"] for p in series["eth0"]] == [100.0]
        assert [p["bytes_sent"] for p in series["wlan0"]] == [10.0]


async def test_the_first_sample_of_an_interface_has_no_rate(device_id):
    async with get_sessionmaker()() as db, db.begin():
        await _sample(db, device_id, at=BASE, sent=12345)
        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))
        # No point at all rather than a point full of nulls: there is nothing to
        # report yet, which is different from reporting an unknown.
        assert series == {}


async def test_the_window_clips_history_rather_than_reaching_behind_it(device_id):
    for i in range(4):
        async with get_sessionmaker()() as db, db.begin():
            await _sample(db, device_id, at=BASE + timedelta(seconds=60 * i), sent=6000 * i)

    async with get_sessionmaker()() as db:
        # Window opens at +90, so only the samples at +120 and +180 are in it.
        series = await interface_rates(db, device_id, BASE + timedelta(seconds=90))

        # One rate, not two: lag() runs over the clipped set, so the first
        # in-window sample has no predecessor to subtract from. The window
        # costs one leading point rather than silently reaching outside itself.
        assert [p["bytes_sent"] for p in series["eth0"]] == [100.0]


async def test_ingest_stores_the_counters(device_id):
    async with get_sessionmaker()() as db, db.begin():
        await apply_telemetry(
            db,
            device_id,
            BASE,
            {
                "cpu_pct": 5.0,
                "nets": [
                    {"name": "eth0", "bytes_sent": 100, "bytes_recv": 200, "err_in": 1},
                    # No name: unusable as a key, must not abort the whole sample.
                    {"bytes_sent": 5},
                ],
            },
        )
        await apply_telemetry(
            db,
            device_id,
            BASE + timedelta(seconds=60),
            {"cpu_pct": 5.0, "nets": [{"name": "eth0", "bytes_sent": 6100, "bytes_recv": 200}]},
        )

        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))
        assert list(series) == ["eth0"]
        assert series["eth0"][0]["bytes_sent"] == pytest.approx(100.0)
        # Unchanged counter is a genuine zero, not a gap.
        assert series["eth0"][0]["bytes_recv"] == pytest.approx(0.0)


async def test_a_counter_too_large_for_bigint_is_dropped_not_fatal(device_id):
    async with get_sessionmaker()() as db, db.begin():
        # uint64 near the top of its range. Postgres bigint is signed, so passing
        # this through would raise and lose the entire telemetry sample over one
        # bad NIC counter.
        await apply_telemetry(
            db,
            device_id,
            BASE,
            {"nets": [{"name": "eth0", "bytes_sent": 2**64 - 1, "bytes_recv": 200}]},
        )
        await apply_telemetry(
            db,
            device_id,
            BASE + timedelta(seconds=60),
            {"nets": [{"name": "eth0", "bytes_sent": 2**64 - 1, "bytes_recv": 6200}]},
        )

        series = await interface_rates(db, device_id, BASE - timedelta(hours=1))
        # The unusable field is null; the usable one alongside it still works.
        assert series["eth0"][0]["bytes_sent"] is None
        assert series["eth0"][0]["bytes_recv"] == pytest.approx(100.0)


async def test_a_duplicate_interface_in_one_sample_does_not_abort_it(device_id):
    async with get_sessionmaker()() as db, db.begin():
        # iface is part of the primary key, so a repeated name in one payload
        # would be a duplicate-key error for the whole batch.
        await apply_telemetry(
            db,
            device_id,
            BASE,
            {"nets": [{"name": "eth0", "bytes_sent": 1}, {"name": "eth0", "bytes_sent": 2}]},
        )
        rows = await db.execute(
            telemetry_network.select().where(telemetry_network.c.device_id == device_id)
        )
        assert len(rows.all()) == 1
