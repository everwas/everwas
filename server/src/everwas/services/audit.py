"""Attributing an audit entry to an organization.

Every AuditLog row carries its own org_id, because the log outlives the things
it describes: the entry for a deleted device is written before the device row
goes, and a reader that reached the organization through that device could not
show it. See everwas.models.audit and migration 0018.

Where the org comes from, in order of preference:

  the actor's, when a user or an API key is acting;
  the subject's, when the entry is about a device or a script;
  a lookup, when the caller holds nothing but a device id.

A writer that leaves it None produces a row no reader can see, which is the
safe direction for that mistake but still a lost record. Prefer passing the
organization down from the caller that already knows it; this lookup is for
the background paths (ingest, the outbox drainer) where there is no caller.
"""

import uuid

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.device import Device


async def device_org(db: AsyncSession, device_id: uuid.UUID) -> uuid.UUID | None:
    """The organization a device belongs to. None if it is already gone.

    One column, no ORM identity map churn: this runs on the ingest path.
    """
    return (
        await db.execute(select(Device.org_id).where(Device.id == device_id))
    ).scalar_one_or_none()
