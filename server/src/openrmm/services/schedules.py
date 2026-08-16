"""Scheduled-run job ids.

Schedules fire from the AGENT's local cache, including while it is offline, so
the server does not assign the job id for a scheduled run the way it does for
every other job. Both sides derive it instead, from the entry and the
unjittered fire time, and they must agree exactly: the id is what a result is
matched against, and a result whose id does not resolve to a row is logged as
"unknown run" and dropped.

This module is the Python half of that derivation. The Go half is
`JobID` in agent/internal/sched/sched.go, and each has a test asserting the
same vector.
"""

import json
import uuid
import zlib
from collections.abc import Sequence
from datetime import datetime

import nats
import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.device import Device
from openrmm.models.script import Script, ScriptSchedule
from openrmm.natsio.agent_request import request_agent
from openrmm.services.jobs import device_matches_target

log = structlog.get_logger()

# uuid5(NAMESPACE_DNS, "schedule.openrmm.invalid"). Part of the wire contract:
# mirrored byte-for-byte by schedNamespace in agent/internal/sched/sched.go.
# Changing it orphans every scheduled result in flight.
SCHED_NAMESPACE = uuid.UUID("06cadeed-8a30-50ab-87f5-7a27b043ba2d")


def scheduled_job_id(entry_id: str, fire_at: datetime) -> uuid.UUID:
    """The job id an agent will report a scheduled run under.

    `fire_at` is the UNJITTERED scheduled time, which is what makes this
    idempotent: the agent spreads its actual start across the window, but two
    reports of the same nominal fire produce the same id, so a redelivery
    updates one run instead of creating a second.
    """
    return uuid.uuid5(SCHED_NAMESPACE, f"{entry_id}:{int(fire_at.timestamp())}")


# --------------------------------------------------------------------------
# The sched.sync document
# --------------------------------------------------------------------------


def entry_for(schedule: ScriptSchedule, script: Script) -> dict:
    """One entry of the document the agent parses (sched.Entry in Go).

    The payload is built from the SCRIPT each time rather than stored on the
    schedule, so editing a script changes what its schedules run. Storing a
    snapshot would leave every schedule quietly executing whatever the body
    was on the day it was created.
    """
    return {
        "entry_id": str(schedule.id),
        "cron": schedule.cron,
        "tz": schedule.tz or "UTC",
        "kind": "script.run",
        "payload": {
            "kind": "script.run",
            "shell": script.shell.value,
            "body": script.body,
            "timeout_s": script.timeout_s,
            "requested_by": f"schedule:{schedule.name or schedule.id}",
        },
        "jitter_s": schedule.jitter_s,
        "misfire_grace_s": schedule.misfire_grace_s,
        "enabled": schedule.enabled,
    }


def schedule_version(entries: list[dict]) -> int:
    """A version derived from the CONTENT of the entries, not a counter.

    The agent reports the version it holds in every heartbeat, and the server
    compares it against this. Because the version IS the content, there is no
    counter to drift, no per-device state to keep, and a device whose entries
    did not change keeps its version when somebody else's schedule is edited.
    Recomputing after a restart gives the same answer, so nothing resyncs the
    fleet just because the dispatcher bounced.

    An empty schedule is version 0, matching what a fresh agent reports before
    it has ever been told anything. Without that special case every agent in a
    fleet with no schedules is one version off for ever and gets an empty
    document pushed to it, which is a NATS round trip per device to say
    nothing.

    Otherwise: positive and under 2^31 so it round-trips through a Go int and
    a JSON number without surprises.
    """
    if not entries:
        return 0
    canonical = json.dumps(entries, sort_keys=True, separators=(",", ":"))
    return zlib.crc32(canonical.encode()) & 0x7FFFFFFF


def build_document(device: Device, schedules: Sequence[tuple[ScriptSchedule, Script]]) -> dict:
    """The full sched.sync payload for one device.

    Only enabled schedules whose target matches are included. A disabled one
    is left out entirely rather than sent with enabled=false: the agent would
    hold an entry it will never fire, and the version would change every time
    somebody toggled a schedule that does not even apply here.
    """
    entries = [
        entry_for(schedule, script)
        for schedule, script in schedules
        if schedule.enabled and device_matches_target(device, schedule.target or {})
    ]
    entries.sort(key=lambda e: e["entry_id"])  # stable, so the version is stable
    return {"schedule_version": schedule_version(entries), "entries": entries}


async def load_schedules(db: AsyncSession) -> list[tuple[ScriptSchedule, Script]]:
    """Every enabled schedule with its script, ready to build documents from."""
    rows = await db.execute(
        select(ScriptSchedule, Script)
        .join(Script, Script.id == ScriptSchedule.script_id)
        .where(ScriptSchedule.enabled)
        .order_by(ScriptSchedule.id)
    )
    return [(schedule, script) for schedule, script in rows.all()]


async def sync_device(
    nc: nats.NATS,
    device: Device,
    schedules: Sequence[tuple[ScriptSchedule, Script]],
) -> int | None:
    """Push the document to one agent. Returns the version it accepted.

    None means the agent did not accept it, which is not an error the caller
    needs to handle: the agent keeps reporting its old version in every
    heartbeat, so the next one tries again. That retry loop is the whole
    reason the version is compared on heartbeat rather than pushed and
    forgotten.
    """
    doc = build_document(device, schedules)
    try:
        raw = await request_agent(
            nc, str(device.id), "sched.sync", json.dumps(doc).encode(), timeout=10
        )
        reply = json.loads(raw)
    except Exception as exc:
        log.warning("sched sync failed", device_id=str(device.id), err=str(exc))
        return None

    if rejected := reply.get("rejected"):
        # Entries the agent refused to schedule: a bad cron, an unknown
        # timezone. The server must NOT go on believing these will fire.
        log.error(
            "agent rejected schedule entries",
            device_id=str(device.id),
            rejected=rejected,
            detail=reply.get("error"),
        )
    if not reply.get("accepted"):
        return None
    log.info(
        "schedule synced",
        device_id=str(device.id),
        version=doc["schedule_version"],
        entries=len(doc["entries"]),
    )
    return doc["schedule_version"]
