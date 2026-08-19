"""Sequenced-amend writer for bitemporal fact tables — the ONLY module allowed
to write fact_hardware / fact_software / fact_patch_state.

Invariants maintained here (and enforced by the GiST exclusion constraints):
- At most one current belief (open recorded_during) per (device_id, fact_key)
  per valid-time instant.
- Beliefs are never mutated: superseding closes recorded_during and inserts
  successor rows. What we believed at any past moment stays reconstructable.
"""

import uuid
from dataclasses import dataclass
from datetime import UTC, datetime

from sqlalchemy import func, select, update
from sqlalchemy.dialects.postgresql import Range
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.facts import FACT_TABLES


class ConcurrentAmendError(RuntimeError):
    """Another writer amended this device's facts mid-flight.

    The design says one dispatcher; this makes the assumption enforced rather
    than assumed, so a second instance produces a retryable error instead of
    silently rewriting history.
    """


@dataclass
class AmendResult:
    added: int = 0
    changed: int = 0
    removed: int = 0
    unchanged: int = 0

    @property
    def wrote(self) -> bool:
        return bool(self.added or self.changed or self.removed)


def _open(lower: datetime) -> Range:
    return Range(lower, None, bounds="[)")


def _closed(lower: datetime, upper: datetime) -> Range:
    return Range(lower, upper, bounds="[)")


# Kinds where an empty snapshot cannot describe a working machine.
#
# The distinction is not "is empty surprising" but "can a functioning host
# genuinely have none of these". A host always has software, always has at
# least a loopback interface, and always has a CPU and an OS.
#
# The kinds NOT listed here go empty as a matter of routine and must never be
# guarded: `patchstate` is empty on a fully patched host, and `logins` is empty
# every single time the last person logs out. Guarding those would refuse the
# most ordinary event each of them has.
EMPTY_IS_IMPLAUSIBLE = frozenset({"software", "network", "hardware"})


class StaleObservationError(Exception):
    """A snapshot observed earlier than a belief it would supersede.

    Two things go wrong when a late snapshot is amended on top of a newer one,
    and both are silent.

    The older value wins the valid-time axis. `correction()` declines to write
    a tombstone because the prior belief starts AFTER observed_at, but the
    prior belief's recorded_during is closed anyway and the older payload is
    inserted as [observed_at, infinity). The newer truth disappears from the
    axis that as_of queries read, which is the axis every incident question
    uses. It is still reachable through knew_at, but only by someone who
    already suspects.

    And where the fact had already changed, a correction row covers [T0, T1).
    A snapshot landing inside that window inserts an overlapping range, the
    GiST exclusion fires, and the whole snapshot is dead-lettered rather than
    just the conflicting key.

    Neither needs a hostile agent. A machine whose RTC is dead, that boots,
    gets corrected by NTP, and flushes its spool produces exactly this, well
    inside MAX_LAG.
    """


class WholesaleRetirementError(Exception):
    """A snapshot would have retired every current fact for a kind.

    Raised rather than returned so it cannot be ignored by a caller that only
    reads AmendResult counts. Ingest converts it to a dead letter with a named
    reason, which is what makes it answerable later.
    """


async def record_facts(
    db: AsyncSession,
    kind: str,
    device_id: uuid.UUID,
    facts: dict[str, dict],
    *,
    observed_at: datetime | None = None,
    source: str = "agent",
) -> AmendResult:
    """Reconcile a full snapshot of facts for one device against current beliefs.

    `facts` maps fact_key -> payload and is treated as COMPLETE for this kind:
    current beliefs absent from it are recorded as ended (a correction row
    closes their valid_during at observed_at).
    """
    model = FACT_TABLES[kind]
    observed_at = observed_at or datetime.now(UTC)
    now = datetime.now(UTC)
    result = AmendResult()

    # Current on BOTH axes. upper_inf(recorded_during) alone is not enough:
    # a correction row is a current *belief* about a value that has already
    # ended, so it has an open recorded_during and a CLOSED valid_during.
    # Including those made a returning fact compare equal to its own tombstone,
    # count as "unchanged", and stay invisible forever. This predicate must
    # match query.get_facts(as_of=None, knew_at=None) exactly.
    rows = (
        await db.execute(
            select(model.id, model.fact_key, model.payload, model.valid_during).where(
                model.device_id == device_id,
                func.upper_inf(model.recorded_during),
                func.upper_inf(model.valid_during),
            )
        )
    ).all()
    current = {r.fact_key: r for r in rows}
    if len(current) != len(rows):
        # Two open beliefs for one key means the exclusion constraint was
        # bypassed or this writer has a bug. Fail loudly rather than silently
        # picking whichever row the heap returned last.
        raise RuntimeError(
            f"{model.__tablename__}: overlapping current beliefs for device {device_id}; "
            "refusing to amend"
        )

    to_close: list[int] = []
    inserts: list[dict] = []

    def insert(fact_key: str, payload: dict, valid: Range) -> None:
        inserts.append(
            {
                "device_id": device_id,
                "fact_key": fact_key,
                "payload": payload,
                "valid_during": valid,
                "recorded_during": _open(now),
                "source": source,
            }
        )

    def correction(prior, fact_key: str) -> None:
        """Re-record the old value with its now-known-finite true window."""
        old_lower = prior.valid_during.lower
        if old_lower is not None and old_lower < observed_at:
            insert(fact_key, prior.payload, _closed(old_lower, observed_at))

    # Refuse an observation older than something we already believe.
    #
    # Bitemporality's promise is that recording a new belief never destroys an
    # old one, and amending on top of a newer observation breaks exactly that.
    # The right place to refuse is here, before any row is written, because the
    # two failure shapes below diverge only in whether a correction row happens
    # to be in the way.
    #
    # Strictly older, not older-or-equal: equal timestamps are ordinary (a
    # re-publish, or two kinds collected in the same cycle) and refusing them
    # would reject normal traffic.
    for key in facts:
        prior = current.get(key)
        if prior is None:
            continue
        prior_lower = prior.valid_during.lower
        if prior_lower is not None and observed_at < prior_lower:
            raise StaleObservationError(
                f"{kind} snapshot for device {device_id} observed at "
                f"{observed_at.isoformat()} is older than the current belief "
                f"about {key}, which starts at {prior_lower.isoformat()}: "
                "amending on top of it would drop the newer value from valid time"
            )

    for key, payload in facts.items():
        prior = current.get(key)
        if prior is None:
            result.added += 1
            insert(key, payload, _open(observed_at))
        elif prior.payload != payload:
            result.changed += 1
            to_close.append(prior.id)
            correction(prior, key)
            insert(key, payload, _open(observed_at))
        else:
            result.unchanged += 1

    for key, prior in current.items():
        if key not in facts:
            result.removed += 1
            to_close.append(prior.id)
            correction(prior, key)

    # Refuse a snapshot that retires EVERYTHING we currently believe.
    #
    # A snapshot is treated as complete, so `{}` is not "no news", it is the
    # assertion that this device now has no packages, no pending patches, no
    # interfaces. The agent is careful never to publish an unverified empty set,
    # but the server must not depend on one publisher's discipline: a truncated
    # body, a collector added later without the rule, or a third-party agent
    # would all erase a device's inventory here.
    #
    # What makes this worth a hard refusal rather than a warning is that the
    # damage looks like legitimate history afterwards. The tombstones are real
    # bitemporal records, so an as_of query agrees the packages ended, and
    # nothing distinguishes the erasure from a genuine mass uninstall.
    #
    # Losing one real mass-removal to a false positive costs one stale kind
    # until the next poll. Accepting one bad empty snapshot costs the device's
    # entire inventory and its history.
    if kind in EMPTY_IS_IMPLAUSIBLE and current and result.removed == len(current) and not facts:
        raise WholesaleRetirementError(
            f"refusing a {kind} snapshot that retires all {result.removed} "
            f"current facts for device {device_id}: an empty snapshot asserts "
            "the device has none, which a failed collector cannot distinguish "
            "itself from"
        )

    if to_close:
        # The ONE permitted UPDATE: closing belief windows, set-based.
        # upper_inf() in the predicate makes this a compare-and-swap. Without
        # it, a concurrent amend that already closed these rows would re-close
        # them, mutating a belief window that is supposed to be immutable.
        closed = await db.execute(
            update(model)
            .where(model.id.in_(to_close), func.upper_inf(model.recorded_during))
            .values(recorded_during=func.tstzrange(func.lower(model.recorded_during), now, "[)"))
        )
        if closed.rowcount != len(to_close):
            # Someone else amended this device between our SELECT and here.
            # Abort rather than write successors on top of their work; the
            # caller's transaction rolls back and the message is retried.
            raise ConcurrentAmendError(
                f"{model.__tablename__}: expected to close {len(to_close)} beliefs, "
                f"closed {closed.rowcount}; concurrent amend for device {device_id}"
            )
    if inserts:
        await db.execute(model.__table__.insert(), inserts)
    return result
