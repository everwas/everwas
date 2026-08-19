"""Per-interface network rates, derived from cumulative counters.

The agent reports counters, not rates: bytes-since-boot, as the kernel keeps
them. Turning those into a throughput chart is a subtraction, and the whole
difficulty is the two cases where the subtraction is wrong.

**Counter resets.** A reboot sets every counter back to zero, and some drivers
still expose 32-bit counters that wrap at 4 GiB. Either way the next sample is
SMALLER than the one before it, and a naive delta is a huge negative number
that becomes a huge positive rate the moment anyone takes an absolute value.
The tempting fix is to assume a wrap and add 2**32 (or 2**64) back, but that
requires knowing the counter width, and there is no way to tell a wrap from a
reboot after the fact. Guess 32-bit on a reboot and you invent a 4 GiB spike;
guess 64-bit and you invent an 18-exabyte one, which flattens every real value
on the chart into a line at the bottom. So a decrease produces NULL: an honest
gap in the series, drawn as a break in the line.

**Gaps.** If the agent was offline for six hours, the two samples either side
of the outage are six hours apart and their delta divided by dt is a six-hour
average. It is arithmetically correct and completely misleading: a busy night
renders as a flat trickle, and the outage itself becomes invisible. Deltas
spanning more than MAX_GAP_S are therefore also NULL, so an outage looks like
what it was.
"""

import uuid
from datetime import datetime

from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

# The agent samples every 60s. Two or three missed samples is jitter worth
# bridging; beyond that the average stops describing any real moment.
MAX_GAP_S = 300

# Counters that become per-second rates.
RATE_FIELDS = (
    "bytes_sent",
    "bytes_recv",
    "packets_sent",
    "packets_recv",
    "err_in",
    "err_out",
    "drop_in",
    "drop_out",
)


def _rate_sql() -> str:
    """Build the per-field delta and rate expressions.

    Written out rather than looped in Python at call time so the statement text
    is stable and Postgres can reuse the plan.
    """
    deltas = ",\n            ".join(f"{f} - lag({f}) OVER w AS d_{f}" for f in RATE_FIELDS)
    rates = ",\n            ".join(
        # dt > 0 guards division; d >= 0 rejects the counter reset; dt <=
        # MAX_GAP_S rejects the smeared average across an outage.
        f"CASE WHEN dt > 0 AND dt <= {MAX_GAP_S} AND d_{f} >= 0 THEN d_{f} / dt END AS {f}_per_s"
        for f in RATE_FIELDS
    )
    return f"""
        WITH deltas AS (
            SELECT
                iface,
                ts,
                -- Cast to float8 so the division below is floating point. Left as
                -- numeric, every rate comes back as an arbitrary-precision
                -- Decimal, which is both slower and prone to values like
                -- Decimal('0E-20') where a plain 0.0 was meant.
                extract(epoch FROM (ts - lag(ts) OVER w))::double precision AS dt,
                {deltas}
            FROM telemetry_network
            WHERE device_id = :device_id AND ts >= :since
            WINDOW w AS (PARTITION BY iface ORDER BY ts)
        )
        SELECT iface, ts, {rates}
        FROM deltas
        -- The first sample of every interface has no predecessor and so no
        -- rate. It is not a gap, there is simply nothing to subtract from.
        WHERE dt IS NOT NULL
        ORDER BY iface, ts
    """


_RATE_SQL_STMT = text(_rate_sql())


async def interface_rates(
    db: AsyncSession, device_id: uuid.UUID, since: datetime
) -> dict[str, list[dict]]:
    """Return per-second rates per interface, newest last.

    Keyed by interface name. A None value in any series is a real gap (reset or
    outage) and should be drawn as a break, not interpolated across.
    """
    rows = await db.execute(_RATE_SQL_STMT, {"device_id": device_id, "since": since})
    series: dict[str, list[dict]] = {}
    for r in rows.mappings():
        point = {"ts": r["ts"]}
        point.update({f: r[f"{f}_per_s"] for f in RATE_FIELDS})
        series.setdefault(r["iface"], []).append(point)
    return series
