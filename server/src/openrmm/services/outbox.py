"""Outbox drainer: delivers queued notifications with bounded retries.

FOR UPDATE SKIP LOCKED is the whole trick. Several dispatchers can drain the
same table concurrently and each one only ever sees rows nobody else holds, so
two drainers never work the same row at the same time.

Delivery is AT-LEAST-ONCE, not exactly-once. An earlier version of this
docstring claimed "a notification is never delivered twice"; it can be. The
send happens before the commit, so a crash (or a lost database connection) in
that window leaves the row pending and the next drainer sends it again. Anyone
building on this should assume duplicates and deduplicate on `alert_id` plus
`kind` if it matters. Losing a page is far worse than sending it twice, which
is why the ordering is that way round.

The other rule here: **nothing is destroyed for a reason an operator can fix**.
A channel that is missing, disabled, or misconfigured puts the row in `blocked`,
which is not terminal. It keeps its place in the queue and delivers once the
configuration is repaired. Only a refusal from the destination itself (a 5xx
mailbox rejection, a 404 webhook) or MAX_ATTEMPTS of genuine transient failure
reaches `failed`, and both are surfaced by outbox_health().
"""

import asyncio
import uuid
from collections import defaultdict
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import case, func, or_, select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.alerting.channels.base import ChannelError, Notification, build_channel
from openrmm.db.engine import session_scope
from openrmm.models.alert import NotificationChannel, NotificationOutbox, OutboxStatus

log = structlog.get_logger()

MAX_ATTEMPTS = 5
BACKOFF_S = (60, 300, 1800, 3600)  # 1m, 5m, 30m, then 1h forever
# A blocked row is waiting on a human, not on a flaky network, so it retries on
# a flat slow cadence and never burns an attempt. It must still retry: the
# operator who re-enables the channel should not also have to replay the queue.
BLOCKED_RETRY_S = 300

# Per-delivery wall-clock ceiling, enforced OUTSIDE the client. httpx timeouts
# are per-operation: an endpoint dribbling one byte every 9 seconds resets the
# read timer forever and holds the request (and the transaction) open.
DELIVERY_TIMEOUT_S = 30.0
# Ceiling for a whole batch. Rows still in flight when it expires are left
# untouched and picked up next pass.
BATCH_TIMEOUT_S = 120.0
# Concurrency, so a critical page does not queue behind 20 slow emails.
MAX_CONCURRENCY = 8
PER_CHANNEL_CONCURRENCY = 2

SEVERITY_ORDER = ("critical", "warning", "info")

# Health thresholds. Silence is the alarm: a queue that is not draining looks
# exactly like a quiet fleet from the outside.
STALE_PENDING_AFTER = timedelta(minutes=15)
FAILED_WINDOW = timedelta(hours=1)

# Statuses a drainer may pick up. `blocked` is deliberately included: it is a
# waiting room, not a grave.
DUE_STATUSES = (OutboxStatus.pending, OutboxStatus.blocked)


def backoff_for(attempts: int) -> int:
    """Delay before the next try, given how many have already failed."""
    return BACKOFF_S[min(max(attempts, 1) - 1, len(BACKOFF_S) - 1)]


def _severity_rank():
    """Order critical first. A page must not wait behind an informational mail."""
    severity = NotificationOutbox.payload["severity"].astext
    return case(
        {name: index for index, name in enumerate(SEVERITY_ORDER)},
        value=severity,
        else_=len(SEVERITY_ORDER),
    )


async def drain_outbox(batch: int = 20) -> int:
    """Deliver one batch of due notifications. Returns how many were processed."""
    async with session_scope() as db:
        rows = list(
            (
                await db.execute(
                    select(NotificationOutbox)
                    .where(
                        NotificationOutbox.status.in_(DUE_STATUSES),
                        NotificationOutbox.next_attempt_at <= func.now(),
                    )
                    .order_by(_severity_rank(), NotificationOutbox.next_attempt_at)
                    .limit(batch)
                    .with_for_update(skip_locked=True)
                )
            ).scalars()
        )
        if not rows:
            return 0

        channel_ids = {row.channel_id for row in rows}
        channels = {
            channel.id: channel
            for channel in (
                await db.execute(
                    select(NotificationChannel).where(NotificationChannel.id.in_(channel_ids))
                )
            ).scalars()
        }
        # Locks are held across delivery on purpose: releasing early would let a
        # second drainer pick up a row that is still in flight. Deliveries run
        # concurrently but touch no database connection (only ORM attributes),
        # so the single session is never used from two tasks at once.
        await _deliver_batch(rows, channels)
        return len(rows)


async def _deliver_batch(
    rows: list[NotificationOutbox], channels: dict[uuid.UUID, NotificationChannel]
) -> None:
    if not rows:
        return  # asyncio.wait() rejects an empty set
    overall = asyncio.Semaphore(MAX_CONCURRENCY)
    per_channel: dict[uuid.UUID, asyncio.Semaphore] = defaultdict(
        lambda: asyncio.Semaphore(PER_CHANNEL_CONCURRENCY)
    )

    async def one(row: NotificationOutbox) -> None:
        channel = channels.get(row.channel_id)
        async with overall, per_channel[row.channel_id]:
            try:
                await asyncio.wait_for(_deliver(row, channel), DELIVERY_TIMEOUT_S)
            except TimeoutError:
                # Transient by construction: we do not know whether it landed.
                _record_failure(
                    row, channel, f"delivery exceeded {DELIVERY_TIMEOUT_S:.0f}s", permanent=False
                )

    tasks = [asyncio.create_task(one(row)) for row in rows]
    # asyncio.wait, not wait_for(gather): a batch that overruns must NOT take
    # down the rows that already finished. Their mutations are pending in this
    # transaction and still have to commit. Rows still in flight were never
    # touched, so they stay due and come back next pass.
    done, pending = await asyncio.wait(tasks, timeout=BATCH_TIMEOUT_S)
    for task in done:
        if (exc := task.exception()) is not None:
            log.error("outbox delivery task crashed", error=f"{type(exc).__name__}: {exc}")
    if pending:
        log.warning(
            "outbox batch exceeded its wall clock",
            limit_s=BATCH_TIMEOUT_S,
            stuck=len(pending),
        )
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)


async def _deliver(row: NotificationOutbox, channel: NotificationChannel | None) -> None:
    if channel is None:
        _record_blocked(row, channel, "channel no longer exists")
        return
    if not channel.enabled:
        _record_blocked(row, channel, "channel is disabled")
        return

    # Construction and payload parsing are CONFIGURATION questions. Failing
    # them used to mark the row failed forever, so ten minutes of maintenance
    # with a channel disabled destroyed every critical notification queued in
    # that window, and one schema bug discarded alerts fleet-wide. They are
    # permanent only for the CURRENT config, which is exactly what `blocked`
    # means.
    try:
        impl = build_channel(str(channel.kind), channel.config or {})
        note = Notification.from_payload(row.payload or {})
    except ChannelError as exc:
        _record_blocked(row, channel, str(exc))
        return
    except Exception as exc:
        _record_blocked(row, channel, f"{type(exc).__name__}: {exc}")
        return

    try:
        await impl.send(note)
    except ChannelError as exc:
        _record_failure(row, channel, str(exc), permanent=exc.permanent)
        return
    except asyncio.CancelledError:
        raise
    except Exception as exc:  # an adapter bug should not stall the queue
        _record_failure(row, channel, f"{type(exc).__name__}: {exc}", permanent=False)
        return

    row.status = OutboxStatus.sent
    row.attempts += 1
    row.last_error = None
    log.info(
        "notification sent",
        outbox_id=str(row.id),
        channel=channel.name,
        kind=str(channel.kind),
        alert_id=str(row.alert_id) if row.alert_id else None,
    )


def _record_blocked(
    row: NotificationOutbox, channel: NotificationChannel | None, error: str
) -> None:
    """Park a row that the current configuration cannot deliver.

    Not terminal, and it does not consume an attempt: whoever fixes the channel
    should get the backlog, not an empty queue and a silent gap.
    """
    row.status = OutboxStatus.blocked
    row.last_error = error[:2000]
    row.next_attempt_at = datetime.now(UTC) + timedelta(seconds=BLOCKED_RETRY_S)
    log.warning(
        "notification blocked on configuration",
        outbox_id=str(row.id),
        channel=channel.name if channel else None,
        channel_id=str(row.channel_id),
        retry_in_s=BLOCKED_RETRY_S,
        error=row.last_error,
    )


def _record_failure(
    row: NotificationOutbox,
    channel: NotificationChannel | None,
    error: str,
    *,
    permanent: bool,
) -> None:
    row.attempts += 1
    row.last_error = error[:2000]
    name = channel.name if channel else None
    if permanent or row.attempts >= MAX_ATTEMPTS:
        row.status = OutboxStatus.failed
        log.warning(
            "notification failed",
            outbox_id=str(row.id),
            channel=name,
            attempts=row.attempts,
            permanent=permanent,
            error=row.last_error,
        )
        return
    row.status = OutboxStatus.pending
    delay = backoff_for(row.attempts)
    row.next_attempt_at = datetime.now(UTC) + timedelta(seconds=delay)
    log.info(
        "notification retry scheduled",
        outbox_id=str(row.id),
        channel=name,
        attempts=row.attempts,
        retry_in_s=delay,
        error=row.last_error,
    )


async def outbox_health(db: AsyncSession, *, now: datetime | None = None) -> dict:
    """Numbers an operator needs to know the notification path is alive.

    Import this from the health endpoint. Nothing else in the system reports
    that alerts were raised but never delivered, and a queue that has stopped
    draining is indistinguishable from a calm night.

    `problems` is empty when healthy; a non-empty list is the reason to return
    503. Never raises on an empty table.
    """
    now = now or datetime.now(UTC)
    counts = dict(
        (
            await db.execute(
                select(NotificationOutbox.status, func.count())
                .group_by(NotificationOutbox.status)
                .where(
                    or_(
                        NotificationOutbox.status != OutboxStatus.failed,
                        NotificationOutbox.created_at > now - FAILED_WINDOW,
                    )
                )
            )
        ).all()
    )
    pending = int(counts.get(OutboxStatus.pending, 0))
    blocked = int(counts.get(OutboxStatus.blocked, 0))
    failed_recent = int(counts.get(OutboxStatus.failed, 0))

    oldest = (
        await db.execute(
            select(func.min(NotificationOutbox.created_at)).where(
                NotificationOutbox.status.in_(DUE_STATUSES)
            )
        )
    ).scalar_one_or_none()
    if oldest is not None and oldest.tzinfo is None:
        oldest = oldest.replace(tzinfo=UTC)
    oldest_age_s = (now - oldest).total_seconds() if oldest is not None else None

    problems: list[str] = []
    if oldest_age_s is not None and oldest_age_s > STALE_PENDING_AFTER.total_seconds():
        problems.append(f"oldest undelivered notification is {oldest_age_s:.0f}s old")
    if blocked:
        problems.append(f"{blocked} notification(s) blocked on channel configuration")
    if failed_recent:
        problems.append(
            f"{failed_recent} notification(s) failed in the last "
            f"{FAILED_WINDOW.total_seconds() / 3600:.0f}h"
        )

    return {
        "pending": pending,
        "blocked": blocked,
        "failed_recent": failed_recent,
        "failed_window_s": int(FAILED_WINDOW.total_seconds()),
        "oldest_pending_age_s": round(oldest_age_s) if oldest_age_s is not None else None,
        "problems": problems,
    }


async def outbox_loop(interval_s: int = 10, batch: int = 20) -> None:
    """Run drain_outbox forever. A full batch loops straight back for the next one."""
    while True:
        processed = 0
        try:
            processed = await drain_outbox(batch)
        except Exception:
            log.exception("outbox drain failed")
        if processed < batch:
            await asyncio.sleep(interval_s)


__all__ = [
    "BACKOFF_S",
    "BLOCKED_RETRY_S",
    "DELIVERY_TIMEOUT_S",
    "MAX_ATTEMPTS",
    "backoff_for",
    "drain_outbox",
    "outbox_health",
    "outbox_loop",
]
