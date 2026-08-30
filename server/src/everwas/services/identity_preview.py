"""What a network-identity policy change would actually do, before it does it.

The setting this previews is safe per machine and dangerous per fleet, and the
words do not change between the two. Setting "never" on one machine by hand is a
considered decision about that machine. Setting it at the top of an organisation
is the same value applied to everything, and every machine currently using one
of our certificates keeps working and then falls off the network as that
certificate expires, one at a time, over the following weeks.

Nothing errors at the moment of the change. The failure arrives a month later,
spread out, and looks like something else entirely.

So this exists to make the consequence visible at the moment somebody is about to
cause it. It is built before the control it protects, deliberately: a preview
added afterwards is one somebody can ship without.

It reads what each device REPORTS holding rather than what we last issued it.
Those are different facts, and the one that matters here is what will actually
stop working. A device that never installed its newest certificate loses network
access when the older one it is really using expires, which is sooner.
"""

from __future__ import annotations

import datetime as dt
import uuid
from dataclasses import dataclass, field

from sqlalchemy import Select, select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.device import Device, DeviceStatus


@dataclass
class AffectedDevice:
    device_id: uuid.UUID
    hostname: str
    #: When this machine stops being able to authenticate. Taken from what it
    #: reports holding, so it is the date that actually matters to it.
    loses_access_at: dt.datetime
    serial: str | None


@dataclass
class Preview:
    """What applying a mode would do. Non-mutating by construction."""

    mode: str
    #: Machines that would lose network access, soonest first, because the
    #: first one to go is the one somebody will be asked about.
    affected: list[AffectedDevice] = field(default_factory=list)
    #: Machines in scope that this would not take offline. Reported as a count
    #: rather than a list: it is context for the number above, not a subject.
    unaffected: int = 0

    @property
    def earliest_loss(self) -> dt.datetime | None:
        return self.affected[0].loses_access_at if self.affected else None

    @property
    def latest_loss(self) -> dt.datetime | None:
        return self.affected[-1].loses_access_at if self.affected else None

    @property
    def safe(self) -> bool:
        """Whether this change takes nothing offline."""
        return not self.affected


async def preview_mode(db: AsyncSession, mode: str, query: Select) -> Preview:
    """Report what setting `mode` across `query`'s devices would do.

    `query` is the caller's already-scoped device selection, so the tenant
    boundary is applied by the caller in the one way this codebase enforces it
    rather than being re-derived here.
    """
    devices = list((await db.execute(query)).scalars().all())
    preview = Preview(mode=mode)

    if mode != "never":
        # auto and always never take a working machine offline. The agent's
        # rule is that detection may never stop it providing for a machine it
        # already provides for, so a machine using our certificate keeps
        # renewing under either. Everything in scope is unaffected.
        preview.unaffected = len(devices)
        return preview

    for device in devices:
        # No reported certificate means either the agent is too old to say or
        # the machine genuinely holds nothing. Neither loses access from this
        # change, and guessing otherwise would inflate the number somebody is
        # about to make a decision on.
        if not device.reported_cert_serial or device.reported_cert_not_after is None:
            preview.unaffected += 1
            continue
        preview.affected.append(
            AffectedDevice(
                device_id=device.id,
                hostname=device.hostname,
                loses_access_at=device.reported_cert_not_after,
                serial=device.reported_cert_serial,
            )
        )

    preview.affected.sort(key=lambda d: d.loses_access_at)
    return preview


def devices_in_scope() -> Select:
    """Devices a policy change would reach.

    Retired devices are excluded. They are meant to stop working, so counting
    them as casualties would pad the number an operator is weighing.
    """
    return select(Device).where(Device.status != DeviceStatus.retired)
