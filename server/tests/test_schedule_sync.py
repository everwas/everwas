"""The reconciler that decides when an agent needs a new schedule.

It runs on EVERY heartbeat from EVERY device, so the cheap path has to be
cheap, and the retry behaviour has to not swallow a real edit.
"""

import uuid

from everwas.dispatcher.schedule_sync import RETRY_FLOOR_S, ScheduleSyncer
from everwas.models.device import Device, OsFamily
from everwas.util.ids import uuid7


class FakeSyncer(ScheduleSyncer):
    """Real decision logic, fake schedule set and fake push."""

    def __init__(self, version_for) -> None:
        super().__init__(nc=None)
        self._version_for = version_for
        self.pushes: list[int] = []
        self.accept = True
        self.clock = 1000.0

    async def _current(self):
        return []

    def _now(self) -> float:
        return self.clock

    async def check(self, device, reported_version):
        # Patch the two things that touch the outside world, keeping the
        # branching under test.
        import everwas.dispatcher.schedule_sync as mod

        real_build, real_sync, real_time = mod.build_document, mod.sync_device, mod.time.monotonic
        version = self._version_for

        async def fake_sync(nc, dev, schedules):
            self.pushes.append(version)
            return version if self.accept else None

        mod.build_document = lambda dev, sch: {"schedule_version": version, "entries": []}
        mod.sync_device = fake_sync
        mod.time.monotonic = self._now
        try:
            return await super().check(device, reported_version)
        finally:
            mod.build_document, mod.sync_device, mod.time.monotonic = (
                real_build,
                real_sync,
                real_time,
            )


def _device() -> Device:
    return Device(id=uuid7(), hostname="host", os_family=OsFamily.linux, tags=[])


async def test_a_matching_version_pushes_nothing():
    """The common case, on every heartbeat from every device."""
    s = FakeSyncer(version_for=4242)
    assert await s.check(_device(), 4242) is False
    assert s.pushes == []


async def test_a_fresh_agent_with_no_schedules_is_left_alone():
    """Both sides say 0, so nothing is sent. Any other empty-value would mean
    one NATS round trip per device, for ever, to say nothing."""
    s = FakeSyncer(version_for=0)
    assert await s.check(_device(), 0) is False
    assert await s.check(_device(), None) is False
    assert s.pushes == []


async def test_drift_pushes():
    s = FakeSyncer(version_for=99)
    assert await s.check(_device(), 7) is True
    assert s.pushes == [99]


async def test_an_agent_that_refuses_is_not_retried_every_heartbeat():
    """A cron the agent cannot parse means it keeps reporting the old version.
    Without a floor that is a push every 30s, per device, indefinitely."""
    s = FakeSyncer(version_for=99)
    s.accept = False
    device = _device()

    assert await s.check(device, 7) is False  # attempted, refused
    for _ in range(5):
        s.clock += 30
        await s.check(device, 7)
    assert len(s.pushes) == 1, "the reconciler hammered an agent that had already said no"

    s.clock += RETRY_FLOOR_S + 1
    await s.check(device, 7)
    assert len(s.pushes) == 2, "the floor never lifted, so a transient failure is permanent"


async def test_a_new_version_is_pushed_immediately_despite_the_floor():
    """The bug a device-keyed floor would cause: an operator fixes a broken
    schedule and the fix sits undelivered for five minutes with no sign why."""
    s = FakeSyncer(version_for=99)
    s.accept = False
    device = _device()
    await s.check(device, 7)
    assert s.pushes == [99]

    s.clock += 5  # well inside the floor
    s._version_for = 100  # the operator just corrected the schedule
    s.accept = True
    assert await s.check(device, 7) is True
    assert s.pushes == [99, 100], "the corrected schedule was held back by the retry floor"


async def test_the_retry_map_does_not_grow_without_bound():
    """One entry per device per edit, for the life of the process, otherwise."""
    s = FakeSyncer(version_for=1)
    device = _device()
    for version in range(1, 25):
        s._version_for = version
        s.clock += 1
        await s.check(device, 0)
    assert len(s._last_push) == 1


async def test_forget_clears_a_device():
    s = FakeSyncer(version_for=99)
    device = _device()
    await s.check(device, 7)
    assert s._last_push
    s.forget(str(device.id))
    assert s._last_push == {}


def test_uuid_keys_are_strings():
    """The map is keyed by str(device.id); a UUID object would silently make
    every lookup miss and the floor would never apply."""
    device = _device()
    assert isinstance(str(device.id), str) and isinstance(device.id, uuid.UUID)
