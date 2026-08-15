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

from openrmm.models.facts import FACT_TABLES


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
