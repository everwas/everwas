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

from openrmm.models.facts import FACT_TABLES


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

    rows = (
        await db.execute(
            select(model.id, model.fact_key, model.payload, model.valid_during).where(
                model.device_id == device_id,
                func.upper_inf(model.recorded_during),
            )
        )
    ).all()
    current = {r.fact_key: r for r in rows}

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

    if to_close:
        # the ONE permitted UPDATE: closing belief windows, set-based
        await db.execute(
            update(model)
            .where(model.id.in_(to_close))
            .values(recorded_during=func.tstzrange(func.lower(model.recorded_during), now, "[)"))
        )
    if inserts:
        await db.execute(model.__table__.insert(), inserts)
    return result
