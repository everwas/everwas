"""Parsing of agent-supplied timestamps.

An endpoint's clock is not trustworthy. It is set by whoever owns the machine,
it drifts, and a dead CMOS battery makes it wrong by years. Two things go
wrong if we take it at face value:

- Telemetry `ts` is the PARTITION KEY. A timestamp outside the partitions that
  exist makes the insert fail, the message nak, and redeliver forever. Proven
  against a live stack: one sample dated a year ahead produced 12 ingest
  failures in 30 seconds and would have continued indefinitely.
- Inventory `ts` becomes `valid_during`. A future-dated fact is invisible to
  every `as_of=now` query, so the device silently reports nothing.

So the wire time is bounded here, at the boundary, and a sample outside the
window is rejected with a log line naming the device rather than being allowed
to poison the pipeline.
"""

from datetime import UTC, datetime, timedelta

import structlog

log = structlog.get_logger()

# A sample may be this far behind (agent spooled it while offline) ...
MAX_LAG = timedelta(hours=36)
# ... and this far ahead (ordinary clock jitter, never more).
MAX_SKEW_AHEAD = timedelta(minutes=5)


class WireTimeError(ValueError):
    """The agent's timestamp is unusable and the message must be dropped."""


def parse_wire_time(raw: str | None, *, now: datetime | None = None) -> datetime:
    """Parse an ISO-8601 timestamp from an agent, or raise WireTimeError."""
    if not raw or not isinstance(raw, str):
        raise WireTimeError("missing ts")
    try:
        ts = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError as exc:
        raise WireTimeError(f"unparseable ts {raw!r}") from exc

    if ts.tzinfo is None:
        # astimezone() would silently interpret this in the SERVER's local
        # zone. In a UTC container that is invisible; on a host set to
        # America/Denver every sample would land 6 hours off, in the wrong
        # partition, with no error anywhere. Offsets are required.
        raise WireTimeError(f"ts {raw!r} has no timezone")

    ts = ts.astimezone(UTC)
    now = now or datetime.now(UTC)
    if ts > now + MAX_SKEW_AHEAD:
        raise WireTimeError(f"ts {ts.isoformat()} is {ts - now} in the future")
    if ts < now - MAX_LAG:
        raise WireTimeError(f"ts {ts.isoformat()} is {now - ts} old")
    return ts
