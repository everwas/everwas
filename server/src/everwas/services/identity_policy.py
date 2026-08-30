"""The organization-level 802.1X identity policy an agent should follow."""

from __future__ import annotations

import uuid

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.device import Device
from everwas.models.org import Organization


async def organization_identity_policy(db: AsyncSession, device_id: uuid.UUID) -> str | None:
    """The policy for the organization this device belongs to, or None.

    None means nobody has decided, and the agent applies its own default. That
    is deliberately distinct from "auto": one is a choice and the other is an
    absence, and an agent that cannot tell them apart cannot report which of
    its machines are running on a policy somebody actually set.

    A device with no organization, or one whose organization has been removed,
    also returns None rather than an error. A renewal is not the place to fail
    over a missing setting: the agent would treat it as a refused renewal, and
    a policy lookup must never be able to cost a machine its credential.
    """
    return (
        await db.execute(
            select(Organization.network_identity)
            .join(Device, Device.org_id == Organization.id)
            .where(Device.id == device_id)
        )
    ).scalar_one_or_none()
