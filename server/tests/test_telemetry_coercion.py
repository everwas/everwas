"""One unstorable number must not destroy a whole telemetry sample.

Found live, not by review: the Windows agent reports load1 = 7.5e-50, because
Windows has no load average and gopsutil returns a meaningless decaying value.
That is a denormal, smaller than the smallest normal float4, so Postgres
rejects the INSERT with NumericValueOutOfRangeError. The whole sample went with
it: cpu, memory, disks, network, and the alert evaluation, six times over, then
dead-lettered. Roughly one sample a minute for hours.

The alert path already learned this lesson and wrote it down (rules.py numeric()
guards against an agent sending {"cpu_pct": "high"}). The insert twelve lines
earlier never got the same treatment.
"""

from datetime import UTC, datetime

import pytest

from openrmm.ingest.telemetry import _real

pytestmark = pytest.mark.usefixtures("pg_database")

WHEN = datetime(2026, 8, 17, 12, tzinfo=UTC)


@pytest.mark.parametrize(
    ("value", "expect"),
    [
        (12.5, 12.5),
        (0, 0.0),
        (None, None),
        # The live failure. Underflow is genuinely indistinguishable from zero
        # at float4 precision, so zero is the honest answer rather than a
        # dropped field.
        (7.535591542908029e-50, 0.0),
        (-1e-45, 0.0),
        # Too large to represent. NOT clamped to the maximum: that would invent
        # a specific enormous reading. Unknown is the truthful answer.
        (1e39, None),
        (-1e39, None),
        (float("inf"), None),
        (float("nan"), None),
        # Wrong type entirely, the shape the alert path already guards.
        ("high", None),
        (True, None),
        ({"n": 1}, None),
    ],
)
def test_real_coercion(value, expect):
    assert _real(value) == expect


async def test_one_unstorable_field_does_not_lose_the_sample():
    import uuid

    from sqlalchemy import select

    from openrmm.db.engine import get_sessionmaker
    from openrmm.ingest.telemetry import apply_telemetry
    from openrmm.models.device import Device, OsFamily
    from openrmm.models.telemetry import telemetry_metrics
    from openrmm.services.partitions import ensure_partitions
    from openrmm.util.ids import uuid7

    async with get_sessionmaker()() as db, db.begin():
        await ensure_partitions(db, retention_days=30)
        d = Device(id=uuid7(), hostname="denormal", os_family=OsFamily.windows, tags=[])
        db.add(d)
        await db.flush()
        device_id = d.id

    ts = datetime.now(UTC).replace(microsecond=0)
    async with get_sessionmaker()() as db, db.begin():
        await apply_telemetry(
            db,
            device_id,
            ts,
            {
                "cpu_pct": 8.6,
                "mem_used": 3263062016,
                "mem_total": 8519069696,
                "swap_pct": 3.2,
                "load1": 7.535591542908029e-50,  # the real value from Windows
                "uptime_s": 46802,
            },
        )

    async with get_sessionmaker()() as db:
        row = (
            await db.execute(
                select(telemetry_metrics).where(telemetry_metrics.c.device_id == device_id)
            )
        ).first()
    assert row is not None, "the sample was lost over one unstorable field"
    assert row.cpu_pct == pytest.approx(8.6, rel=1e-3)
    assert row.uptime_s == 46802
    assert row.load1 == 0.0
