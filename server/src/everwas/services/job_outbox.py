"""Job-outbox drainer: publishes committed jobs to the durable JOBS stream.

The same shape as services/outbox.py, for the same reason. FOR UPDATE SKIP
LOCKED is the whole trick: several dispatchers can drain concurrently and each
only ever sees rows nobody else holds.

Delivery is at-least-once on purpose. If a publish succeeds and the
bookkeeping transaction then fails, the row stays pending and is published
again; `Nats-Msg-Id` is the job id, so JetStream drops the duplicate. Marking
first and publishing second would be at-most-once, which for job dispatch
means silently losing work an operator was told had been queued.
"""

import asyncio
from datetime import UTC, datetime, timedelta

import nats.js
import structlog
from sqlalchemy import func, select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.db.engine import session_scope
from everwas.models.audit import ActorType, AuditLog
from everwas.models.job_outbox import (
    KIND_PATCH_INSTALL,
    KIND_SCRIPT_RUN,
    JobOutbox,
    JobOutboxStatus,
)
from everwas.models.patch import PatchJob, PatchJobStatus
from everwas.models.script import RunStatus, ScriptRun
from everwas.services.audit import device_org
from everwas.services.jobs import job_envelope

log = structlog.get_logger()

MAX_ATTEMPTS = 8
BACKOFF_S = (5, 15, 60, 300, 900)  # 5s, 15s, 1m, 5m, then 15m until we give up
PUBLISH_TIMEOUT_S = 5


def backoff_for(attempts: int) -> int:
    """Delay before the next try, given how many have already failed."""
    return BACKOFF_S[min(max(attempts, 1) - 1, len(BACKOFF_S) - 1)]


async def drain_job_outbox(js: nats.js.JetStreamContext, batch: int = 50) -> int:
    """Publish one batch of due jobs. Returns how many rows were touched."""
    async with session_scope() as db:
        rows = list(
            (
                await db.execute(
                    select(JobOutbox)
                    .where(
                        JobOutbox.status == JobOutboxStatus.pending,
                        JobOutbox.next_attempt_at <= func.now(),
                    )
                    .order_by(JobOutbox.next_attempt_at)
                    .limit(batch)
                    .with_for_update(skip_locked=True)
                )
            ).scalars()
        )
        if not rows:
            return 0

        # Locks are held across publish on purpose: releasing early would let a
        # second drainer pick up a row that is still in flight.
        handled = 0
        for row in rows:
            handled += 1
            if not await _publish(db, js, row):
                # A publish failure is almost always "the broker is unwell",
                # which the rest of this batch would only rediscover one
                # timeout at a time. Leave them pending and untouched; they are
                # still due, so the next tick picks them up first.
                break
        return handled


async def _publish(db: AsyncSession, js: nats.js.JetStreamContext, row: JobOutbox) -> bool:
    try:
        await js.publish(
            row.subject,
            job_envelope(str(row.device_id), str(row.id), row.kind, row.payload or {}),
            headers={"Nats-Msg-Id": str(row.id)},  # dedup on redelivery
            timeout=PUBLISH_TIMEOUT_S,
        )
    except Exception as exc:
        await _record_failure(db, row, f"{type(exc).__name__}: {exc}")
        return False

    row.status = JobOutboxStatus.published
    row.attempts += 1
    row.published_at = datetime.now(UTC)
    row.last_error = None
    log.info(
        "job published",
        job_id=str(row.id),
        kind=row.kind,
        device_id=str(row.device_id),
        attempts=row.attempts,
    )
    return True


async def _record_failure(db: AsyncSession, row: JobOutbox, error: str) -> None:
    row.attempts += 1
    row.last_error = error[:2000]
    if row.attempts < MAX_ATTEMPTS:
        delay = backoff_for(row.attempts)
        row.next_attempt_at = datetime.now(UTC) + timedelta(seconds=delay)
        log.warning(
            "job publish retry scheduled",
            job_id=str(row.id),
            kind=row.kind,
            attempts=row.attempts,
            retry_in_s=delay,
            error=row.last_error,
        )
        return

    row.status = JobOutboxStatus.failed
    reason = f"never dispatched after {row.attempts} attempts: {row.last_error}"
    await _fail_owner(db, row, reason)
    db.add(
        AuditLog(
            org_id=await device_org(db, row.device_id),
            actor_type=ActorType.system,
            actor_id="job-outbox",
            action="job.dispatch_failed",
            target_type="device",
            target_id=str(row.device_id),
            detail={"job_id": str(row.id), "kind": row.kind, "error": row.last_error},
        )
    )
    log.error(
        "job never dispatched",
        job_id=str(row.id),
        kind=row.kind,
        device_id=str(row.device_id),
        attempts=row.attempts,
        error=row.last_error,
    )


async def _fail_owner(db: AsyncSession, row: JobOutbox, reason: str) -> None:
    """Tell the truth in the row an operator actually looks at.

    Without this the UI shows "queued" forever for work that will never be
    sent, which is the same lie the inline publish told, only slower.
    """
    if row.kind == KIND_SCRIPT_RUN:
        run = (
            await db.execute(select(ScriptRun).where(ScriptRun.id == row.id))
        ).scalar_one_or_none()
        if run is None or run.status not in (RunStatus.queued, RunStatus.delivered):
            return
        run.status = RunStatus.failed
        run.finished_at = datetime.now(UTC)
        run.stderr = f"{run.stderr}\neverwas: {reason}".strip()
    elif row.kind == KIND_PATCH_INSTALL:
        job = (await db.execute(select(PatchJob).where(PatchJob.id == row.id))).scalar_one_or_none()
        if job is None or job.status is not PatchJobStatus.queued:
            return
        job.status = PatchJobStatus.failed
        job.finished_at = datetime.now(UTC)
        job.log = f"{job.log}\neverwas: {reason}".strip()
        job.failed = {ext: "not dispatched" for ext in (job.external_ids or [])}
    # patch.scan has no owner row: the failed outbox row and the audit entry
    # are the whole record, which is all a scan request ever was.


async def job_outbox_loop(
    js: nats.js.JetStreamContext, interval_s: int = 2, batch: int = 50
) -> None:
    """Run drain_job_outbox forever. A full batch loops straight back."""
    while True:
        processed = 0
        try:
            processed = await drain_job_outbox(js, batch)
        except Exception:
            log.exception("job outbox drain failed")
        if processed < batch:
            await asyncio.sleep(interval_s)


__all__ = [
    "BACKOFF_S",
    "MAX_ATTEMPTS",
    "backoff_for",
    "drain_job_outbox",
    "job_outbox_loop",
]
