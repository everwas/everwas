"""Outbox drainer: delivers queued notifications with bounded retries.

FOR UPDATE SKIP LOCKED is the whole trick. Several dispatchers can drain the
same table concurrently and each one only ever sees rows nobody else holds, so
a notification is never delivered twice.
"""

import asyncio
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import func, select

from openrmm.alerting.channels.base import ChannelError, Notification, build_channel
from openrmm.db.engine import session_scope
from openrmm.models.alert import NotificationChannel, NotificationOutbox, OutboxStatus

log = structlog.get_logger()

MAX_ATTEMPTS = 5
BACKOFF_S = (60, 300, 1800, 3600)  # 1m, 5m, 30m, then 1h forever


def backoff_for(attempts: int) -> int:
    """Delay before the next try, given how many have already failed."""
    return BACKOFF_S[min(max(attempts, 1) - 1, len(BACKOFF_S) - 1)]


async def drain_outbox(batch: int = 20) -> int:
    """Deliver one batch of due notifications. Returns how many were processed."""
    async with session_scope() as db:
        rows = list(
            (
                await db.execute(
                    select(NotificationOutbox)
                    .where(
                        NotificationOutbox.status == OutboxStatus.pending,
                        NotificationOutbox.next_attempt_at <= func.now(),
                    )
                    .order_by(NotificationOutbox.next_attempt_at)
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
        # second drainer pick up a row that is still in flight.
        for row in rows:
            await _deliver(row, channels.get(row.channel_id))
        return len(rows)


async def _deliver(row: NotificationOutbox, channel: NotificationChannel | None) -> None:
    if channel is None or not channel.enabled:
        row.status = OutboxStatus.failed
        row.last_error = "channel is missing or disabled"
        log.warning("notification dropped", outbox_id=str(row.id), reason=row.last_error)
        return

    try:
        impl = build_channel(str(channel.kind), channel.config or {})
        note = Notification.from_payload(row.payload or {})
        await impl.send(note)
    except ChannelError as exc:
        _record_failure(row, channel, str(exc), permanent=exc.permanent)
        return
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


def _record_failure(
    row: NotificationOutbox, channel: NotificationChannel, error: str, *, permanent: bool
) -> None:
    row.attempts += 1
    row.last_error = error[:2000]
    if permanent or row.attempts >= MAX_ATTEMPTS:
        row.status = OutboxStatus.failed
        log.warning(
            "notification failed",
            outbox_id=str(row.id),
            channel=channel.name,
            attempts=row.attempts,
            permanent=permanent,
            error=row.last_error,
        )
        return
    delay = backoff_for(row.attempts)
    row.next_attempt_at = datetime.now(UTC) + timedelta(seconds=delay)
    log.info(
        "notification retry scheduled",
        outbox_id=str(row.id),
        channel=channel.name,
        attempts=row.attempts,
        retry_in_s=delay,
        error=row.last_error,
    )


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


__all__ = ["BACKOFF_S", "MAX_ATTEMPTS", "backoff_for", "drain_outbox", "outbox_loop"]
