"""Read side of the bitemporal fact tables.

Two axes, two parameters:
- as_of: valid time — "what was true on the machine at T?"
- knew_at: record time — "according to what we knew at T?" (None = per our
  current knowledge)
"""

import uuid
from datetime import datetime

from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.facts import FACT_TABLES


async def get_facts(
    db: AsyncSession,
    kind: str,
    device_id: uuid.UUID,
    *,
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> list[dict]:
    model = FACT_TABLES[kind]
    conditions = [model.device_id == device_id]

    if knew_at is None:
        conditions.append(func.upper_inf(model.recorded_during))
    else:
        conditions.append(model.recorded_during.contains(knew_at))

    if as_of is None:
        conditions.append(func.upper_inf(model.valid_during))
    else:
        conditions.append(model.valid_during.contains(as_of))

    rows = await db.execute(
        select(model.fact_key, model.payload, model.valid_during, model.source)
        .where(*conditions)
        .order_by(model.fact_key)
    )
    return [
        {
            "fact_key": r.fact_key,
            "payload": r.payload,
            "valid_from": r.valid_during.lower,
            "valid_to": r.valid_during.upper,
            "source": r.source,
        }
        for r in rows
    ]


async def diff_facts(
    db: AsyncSession,
    kind: str,
    device_id: uuid.UUID,
    *,
    from_ts: datetime,
    to_ts: datetime,
    knew_at: datetime | None = None,
) -> dict[str, list[dict]]:
    """What changed on this device between two moments in valid time.

    Answers "what did this machine gain, lose, or upgrade last week" using our
    current knowledge, or the knowledge we had at `knew_at`.
    """
    before = {
        f["fact_key"]: f["payload"]
        for f in await get_facts(db, kind, device_id, as_of=from_ts, knew_at=knew_at)
    }
    after = {
        f["fact_key"]: f["payload"]
        for f in await get_facts(db, kind, device_id, as_of=to_ts, knew_at=knew_at)
    }

    added = [{"fact_key": k, "payload": v} for k, v in after.items() if k not in before]
    removed = [{"fact_key": k, "payload": v} for k, v in before.items() if k not in after]
    changed = [
        {"fact_key": k, "before": before[k], "after": v}
        for k, v in after.items()
        if k in before and before[k] != v
    ]
    return {
        "added": sorted(added, key=lambda f: f["fact_key"]),
        "removed": sorted(removed, key=lambda f: f["fact_key"]),
        "changed": sorted(changed, key=lambda f: f["fact_key"]),
    }
