"""Keep each agent's schedule cache in step with the database.

Reconciliation, not push-and-forget. Every heartbeat carries the
`schedule_version` the agent is actually holding; the server computes what that
device's version SHOULD be and pushes a new document when they differ.

That inverts the usual failure mode. A push on change is lost whenever the
agent is offline, mid-restart, or the request times out, and nothing ever
notices: the schedule silently never runs on that machine, which is exactly
the failure an operator cannot see. Comparing on every heartbeat means a
device that missed an update fixes itself within one heartbeat of coming back,
and a device that is already correct costs a dictionary lookup.

The version is derived from the entries themselves (see
openrmm.services.schedules.schedule_version), so there is no counter to drift
and a dispatcher restart does not resync the fleet.
"""

import asyncio
import time

import nats
import structlog

from openrmm.db.engine import session_scope
from openrmm.models.device import Device
from openrmm.services.schedules import build_document, load_schedules, sync_device

log = structlog.get_logger()

#: How long the schedule set is cached before being re-read. The check runs on
#: every heartbeat from every device, so this must not be a query each time.
CACHE_TTL_S = 30

#: Do not retry the SAME version to the same device more often than this. An
#: agent that refuses a document (a cron it cannot parse, a full disk) keeps
#: reporting the old version, and without a floor the server would push again
#: on every heartbeat for ever.
#:
#: Keyed on the version, not just the device: a floor on the device alone
#: would swallow the next genuine edit, so fixing a broken schedule would sit
#: undelivered for five minutes with no sign of why.
RETRY_FLOOR_S = 300


class ScheduleSyncer:
    """Decides, per heartbeat, whether an agent needs a new schedule."""

    def __init__(self, nc: nats.NATS) -> None:
        self._nc = nc
        self._schedules: list = []
        self._loaded_at = 0.0
        # (device_id, version) -> when we last tried to push that version.
        self._last_push: dict[tuple[str, int], float] = {}
        self._lock = asyncio.Lock()

    def invalidate(self) -> None:
        """Force a reload on the next check. Called when a schedule changes."""
        self._loaded_at = 0.0

    async def _current(self) -> list:
        now = time.monotonic()
        if now - self._loaded_at < CACHE_TTL_S:
            return self._schedules
        async with self._lock:
            # Re-check under the lock: a burst of heartbeats would otherwise
            # all decide to reload at once.
            if time.monotonic() - self._loaded_at < CACHE_TTL_S:
                return self._schedules
            async with session_scope() as db:
                self._schedules = await load_schedules(db)
            self._loaded_at = time.monotonic()
        return self._schedules

    async def check(self, device: Device, reported_version: int | None) -> bool:
        """Push a document if the agent is out of step. Returns True if pushed.

        A missing version means an agent old enough not to report one. It is
        treated as 0, so it converges to whatever it should have rather than
        being left alone for ever.
        """
        schedules = await self._current()
        want = build_document(device, schedules)["schedule_version"]
        have = int(reported_version or 0)

        # An empty schedule is version 0 too, so a device with nothing to run
        # and an agent reporting nothing agree, and no document is sent.
        if have == want:
            return False

        key = (str(device.id), want)
        now = time.monotonic()
        if now - self._last_push.get(key, 0.0) < RETRY_FLOOR_S:
            return False
        self._last_push[key] = now
        # A device only ever needs the newest version, so older attempts are
        # dead weight. Without this the map grows for the life of the process,
        # one entry per device per edit.
        self._last_push = {
            (dev, ver): at
            for (dev, ver), at in self._last_push.items()
            if dev != key[0] or ver == want
        }

        log.info(
            "schedule out of step",
            device_id=key[0],
            agent_has=have,
            server_wants=want,
        )
        return await sync_device(self._nc, device, schedules) is not None

    def forget(self, device_id: str) -> None:
        self._last_push = {k: v for k, v in self._last_push.items() if k[0] != device_id}


# The API process edits schedules; the dispatcher holds the cache. They are
# separate processes, so the API cannot reach in and invalidate directly. The
# CACHE_TTL_S reload is what actually carries a change across, and this hook is
# for the in-process case (tests, and a future single-process mode).
_SYNCER: ScheduleSyncer | None = None


def set_syncer(syncer: ScheduleSyncer) -> None:
    global _SYNCER
    _SYNCER = syncer


def invalidate_schedule_cache() -> None:
    if _SYNCER is not None:
        _SYNCER.invalidate()
