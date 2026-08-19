"""Outbox delivery: what may be destroyed, what must merely wait, and what
happens when one endpoint is slow.

The defect these cover: every configuration problem used to be terminal. A
channel disabled for ten minutes of maintenance marked every notification
queued in that window `failed`, forever, silently. The operator re-enabled the
channel and got an empty queue, which looks exactly like a quiet night.
"""

import asyncio
import time
import uuid
from datetime import UTC, datetime, timedelta

import pytest

from everwas.alerting.channels.base import ChannelError
from everwas.models.alert import (
    ChannelKind,
    NotificationChannel,
    NotificationOutbox,
    OutboxStatus,
)
from everwas.services import outbox as outbox_mod
from everwas.services.outbox import (
    MAX_ATTEMPTS,
    _deliver,
    _deliver_batch,
    drain_outbox,
    outbox_health,
)

PAYLOAD = {
    "kind": "alert.firing",
    "title": "CPU high on web-01",
    "body": "cpu_pct 99 over threshold 90",
    "severity": "critical",
    "context": {},
}


def a_row(*, channel_id: uuid.UUID | None = None, payload: dict | None = None, **kw):
    """A detached outbox row. Defaults are set explicitly: the model's column
    defaults only apply at flush time, and these never touch a session."""
    return NotificationOutbox(
        id=uuid.uuid4(),
        channel_id=channel_id or uuid.uuid4(),
        payload=PAYLOAD if payload is None else payload,
        status=kw.pop("status", OutboxStatus.pending),
        attempts=kw.pop("attempts", 0),
        next_attempt_at=kw.pop("next_attempt_at", datetime.now(UTC)),
        **kw,
    )


def a_channel(*, enabled: bool = True, config: dict | None = None, name: str = "ops"):
    return NotificationChannel(
        id=uuid.uuid4(),
        name=name,
        kind=ChannelKind.webhook,
        config={"url": "https://hooks.example.com/rmm"} if config is None else config,
        enabled=enabled,
    )


class Fake:
    """A channel adapter whose behaviour the test dictates."""

    def __init__(self, *, raises: Exception | None = None, delay: float = 0.0, log=None):
        self.raises = raises
        self.delay = delay
        self.log = log

    async def send(self, note):
        if self.log is not None:
            self.log.append(note.severity)
        if self.delay:
            await asyncio.sleep(self.delay)
        if self.raises:
            raise self.raises


def install(monkeypatch, factory):
    monkeypatch.setattr(outbox_mod, "build_channel", lambda kind, config: factory())


# --- H5: a configuration problem must DEFER, never destroy -------------------


async def test_a_disabled_channel_defers_instead_of_destroying():
    """Ten minutes of maintenance must not eat every critical page queued in it."""
    row = a_row()
    before = datetime.now(UTC)
    await _deliver(row, a_channel(enabled=False))

    assert row.status is OutboxStatus.blocked, "a disabled channel destroyed the notification"
    assert row.attempts == 0, "waiting on a human must not burn a delivery attempt"
    assert row.next_attempt_at > before, "blocked rows must come back"
    assert "disabled" in row.last_error


async def test_a_missing_channel_defers_instead_of_destroying():
    row = a_row()
    await _deliver(row, None)
    assert row.status is OutboxStatus.blocked
    assert row.attempts == 0


async def test_a_misconfigured_channel_defers():
    """Permanent for the CURRENT config is not permanent."""
    row = a_row()
    await _deliver(row, a_channel(config={}))  # webhook with no url
    assert row.status is OutboxStatus.blocked
    assert "url" in row.last_error


async def test_a_malformed_payload_defers():
    """A schema bug would otherwise discard alerts fleet-wide, in silence."""
    row = a_row(payload={"nope": True})
    await _deliver(row, a_channel())
    assert row.status is OutboxStatus.blocked
    assert row.attempts == 0


async def test_blocked_rows_are_delivered_once_the_channel_comes_back(monkeypatch):
    sent = []
    install(monkeypatch, lambda: Fake(log=sent))
    row = a_row()
    await _deliver(row, a_channel(enabled=False))
    assert row.status is OutboxStatus.blocked

    row.next_attempt_at = datetime.now(UTC) - timedelta(seconds=1)
    await _deliver(row, a_channel(enabled=True))
    assert row.status is OutboxStatus.sent
    assert sent == ["critical"]


# --- ...but a refusal from the destination is still terminal -----------------


async def test_a_refusal_from_the_destination_is_still_permanent(monkeypatch):
    """The fix must not turn every dead endpoint into an immortal retry."""
    install(monkeypatch, lambda: Fake(raises=ChannelError("webhook returned 404", permanent=True)))
    row = a_row()
    await _deliver(row, a_channel())
    assert row.status is OutboxStatus.failed
    assert row.attempts == 1


async def test_a_transient_failure_retries_with_backoff(monkeypatch):
    install(monkeypatch, lambda: Fake(raises=ChannelError("connection refused")))
    row = a_row()
    await _deliver(row, a_channel())
    assert row.status is OutboxStatus.pending
    assert row.attempts == 1
    assert row.next_attempt_at > datetime.now(UTC)


async def test_transient_failures_give_up_eventually(monkeypatch):
    install(monkeypatch, lambda: Fake(raises=ChannelError("connection refused")))
    row = a_row(attempts=MAX_ATTEMPTS - 1)
    await _deliver(row, a_channel())
    assert row.status is OutboxStatus.failed


# --- H6: one slow endpoint must not hold everything else ---------------------


async def test_a_slow_endpoint_is_cut_off_by_a_total_wall_clock(monkeypatch):
    """httpx timeouts are per-operation. A dribbling server resets them forever."""
    monkeypatch.setattr(outbox_mod, "DELIVERY_TIMEOUT_S", 0.05)
    install(monkeypatch, lambda: Fake(delay=30.0))
    channel = a_channel()
    row = a_row(channel_id=channel.id)

    # The bound is the thing under test, so the test must not rely on it: if
    # delivery is unbounded this fails instead of hanging the suite.
    await asyncio.wait_for(_deliver_batch([row], {channel.id: channel}), 5.0)

    assert row.status is OutboxStatus.pending
    assert row.attempts == 1
    assert "exceeded" in row.last_error


async def test_the_batch_is_delivered_concurrently(monkeypatch):
    """A critical page must not queue behind twenty slow emails."""
    install(monkeypatch, lambda: Fake(delay=0.3))
    channel = a_channel()
    rows = [a_row(channel_id=channel.id) for _ in range(4)]

    started = time.monotonic()
    await _deliver_batch(rows, {channel.id: channel})
    elapsed = time.monotonic() - started

    assert all(row.status is OutboxStatus.sent for row in rows)
    assert elapsed < 0.9, f"delivery is serial ({elapsed:.2f}s for 4 x 0.3s)"


async def test_a_stuck_delivery_does_not_discard_the_rows_that_finished(monkeypatch):
    monkeypatch.setattr(outbox_mod, "BATCH_TIMEOUT_S", 0.2)
    monkeypatch.setattr(outbox_mod, "DELIVERY_TIMEOUT_S", 30.0)
    # one adapter per row, in row order: the first returns at once, the second
    # never does
    delays = iter([0.0, 30.0])
    monkeypatch.setattr(outbox_mod, "build_channel", lambda kind, config: Fake(delay=next(delays)))

    channel = a_channel()
    quick, stuck = a_row(channel_id=channel.id), a_row(channel_id=channel.id)

    await asyncio.wait_for(_deliver_batch([quick, stuck], {channel.id: channel}), 5.0)

    assert quick.status is OutboxStatus.sent, "a stuck sibling discarded a completed delivery"
    assert stuck.status is OutboxStatus.pending, "an unfinished row must stay due"
    assert stuck.attempts == 0


# --- everything below needs the database -------------------------------------


@pytest.mark.usefixtures("pg_database")
class TestAgainstTheDatabase:
    async def _seed(self, channel: NotificationChannel, rows: list[tuple[str, datetime]]) -> None:
        from everwas.db.engine import get_sessionmaker

        async with get_sessionmaker()() as db, db.begin():
            db.add(channel)
            await db.flush()
            for severity, created_at in rows:
                db.add(
                    NotificationOutbox(
                        id=uuid.uuid4(),
                        channel_id=channel.id,
                        payload={**PAYLOAD, "severity": severity, "title": severity},
                        status=OutboxStatus.pending,
                        attempts=0,
                        next_attempt_at=created_at,
                        created_at=created_at,
                    )
                )

    async def test_critical_is_drained_before_older_informational_traffic(self, monkeypatch):
        """Ordering by next_attempt_at alone put the page behind the backlog."""
        seen: list[str] = []
        monkeypatch.setattr(outbox_mod, "MAX_CONCURRENCY", 1)
        monkeypatch.setattr(outbox_mod, "PER_CHANNEL_CONCURRENCY", 1)
        install(monkeypatch, lambda: Fake(log=seen))

        channel = a_channel(name=f"ops-{uuid.uuid4().hex[:6]}")
        old = datetime.now(UTC) - timedelta(minutes=10)
        await self._seed(
            channel,
            [
                ("info", old),
                ("info", old + timedelta(seconds=1)),
                ("warning", old + timedelta(seconds=2)),
                ("critical", datetime.now(UTC) - timedelta(seconds=1)),
            ],
        )

        assert await drain_outbox(batch=10) == 4
        assert seen[0] == "critical", f"the page queued behind the backlog: {seen}"

    async def test_a_disabled_channel_survives_a_full_drain_cycle(self, monkeypatch):
        """End to end: disable, queue, re-enable, and the page still arrives."""
        from sqlalchemy import select, update

        from everwas.db.engine import get_sessionmaker

        sent: list[str] = []
        install(monkeypatch, lambda: Fake(log=sent))
        channel = a_channel(name=f"ops-{uuid.uuid4().hex[:6]}", enabled=False)
        await self._seed(channel, [("critical", datetime.now(UTC) - timedelta(seconds=1))])

        assert await drain_outbox(batch=10) == 1
        async with get_sessionmaker()() as db:
            row = (await db.execute(select(NotificationOutbox))).scalar_one()
            assert row.status is OutboxStatus.blocked
            assert sent == []

        # the operator finishes maintenance
        async with get_sessionmaker()() as db, db.begin():
            await db.execute(
                update(NotificationChannel)
                .where(NotificationChannel.id == channel.id)
                .values(enabled=True)
            )
            await db.execute(update(NotificationOutbox).values(next_attempt_at=datetime.now(UTC)))

        assert await drain_outbox(batch=10) == 1
        async with get_sessionmaker()() as db:
            row = (await db.execute(select(NotificationOutbox))).scalar_one()
        assert row.status is OutboxStatus.sent, "the backlog was lost while the channel was off"
        assert sent == ["critical"]

    async def test_outbox_health_reports_a_queue_that_is_not_draining(self):
        from everwas.db.engine import get_sessionmaker

        channel = a_channel(name=f"ops-{uuid.uuid4().hex[:6]}")
        stale = datetime.now(UTC) - timedelta(minutes=45)
        await self._seed(channel, [("critical", stale), ("info", datetime.now(UTC))])
        async with get_sessionmaker()() as db, db.begin():
            db.add(
                NotificationOutbox(
                    id=uuid.uuid4(),
                    channel_id=channel.id,
                    payload=PAYLOAD,
                    status=OutboxStatus.failed,
                    attempts=5,
                    next_attempt_at=datetime.now(UTC),
                    created_at=datetime.now(UTC),
                )
            )
            db.add(
                NotificationOutbox(
                    id=uuid.uuid4(),
                    channel_id=channel.id,
                    payload=PAYLOAD,
                    status=OutboxStatus.failed,
                    attempts=5,
                    next_attempt_at=datetime.now(UTC),
                    created_at=datetime.now(UTC) - timedelta(days=2),
                )
            )

        async with get_sessionmaker()() as db:
            health = await outbox_health(db)

        assert health["pending"] == 2
        assert health["failed_recent"] == 1, "the 2-day-old failure is outside the window"
        assert health["oldest_pending_age_s"] >= 45 * 60
        assert health["problems"], "a 45 minute old undelivered page is not healthy"

    async def test_outbox_health_is_quiet_on_an_empty_queue(self):
        from everwas.db.engine import get_sessionmaker

        async with get_sessionmaker()() as db:
            health = await outbox_health(db)
        assert health == {
            "pending": 0,
            "blocked": 0,
            "failed_recent": 0,
            "failed_window_s": 3600,
            "oldest_pending_age_s": None,
            "problems": [],
        }
