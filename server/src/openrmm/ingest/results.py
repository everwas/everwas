"""Job output/result ingest -> script_runs rows."""

import base64
import json
import uuid
from datetime import UTC, datetime

import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.device import Device
from openrmm.models.script import RunStatus, RunTrigger, ScriptRun, ScriptSchedule

log = structlog.get_logger()

MAX_STORED_OUTPUT = 1_000_000  # per stream, per run


def _parse(subject: str, payload: bytes, suffix: str) -> tuple[uuid.UUID, uuid.UUID, dict] | None:
    """agents.{agent_id}.jobs.{job_id}.{suffix} -> (device_id, job_id, data)."""
    parts = subject.split(".")
    if len(parts) != 5 or parts[2] != "jobs" or parts[4] != suffix:
        return None
    try:
        device_id = uuid.UUID(parts[1])
        job_id = uuid.UUID(parts[3])
        envelope = json.loads(payload)
    except (ValueError, json.JSONDecodeError):
        return None
    data = envelope.get("data") if isinstance(envelope, dict) else None
    return device_id, job_id, data or {}


def parse_job_output(subject: str, payload: bytes):
    return _parse(subject, payload, "output")


def parse_job_result(subject: str, payload: bytes):
    return _parse(subject, payload, "result")


async def _run_for(db: AsyncSession, job_id: uuid.UUID, device_id: uuid.UUID) -> ScriptRun | None:
    run = (await db.execute(select(ScriptRun).where(ScriptRun.id == job_id))).scalar_one_or_none()
    if run is None or run.device_id != device_id:
        return None
    return run


async def apply_job_output(
    db: AsyncSession, device_id: uuid.UUID, job_id: uuid.UUID, data: dict
) -> None:
    run = await _run_for(db, job_id, device_id)
    if run is None:
        # Output arrives BEFORE the result, so a scheduled run's row does not
        # exist yet. Adopting here rather than waiting is the difference
        # between having a nightly job's output and having only its exit code.
        run = await _adopt_scheduled_run(db, device_id, job_id, data)
    if run is None:
        return
    try:
        chunk = base64.b64decode(data.get("data") or "").decode("utf-8", errors="replace")
    except ValueError:
        log.warning("bad output chunk", job_id=str(job_id))
        return

    if run.status in (RunStatus.queued, RunStatus.delivered):
        run.status = RunStatus.running
        run.started_at = run.started_at or datetime.now(UTC)

    if data.get("stream") == "stderr":
        if len(run.stderr) < MAX_STORED_OUTPUT:
            run.stderr += chunk
    elif len(run.stdout) < MAX_STORED_OUTPUT:
        run.stdout += chunk
    else:
        run.truncated = True


async def _apply_patch_job_result(
    db: AsyncSession, device_id: uuid.UUID, job_id: uuid.UUID, data: dict
) -> bool:
    """Patch jobs share the job_id namespace with script runs; try them too."""
    from openrmm.models.patch import PatchJob, PatchJobStatus

    job = (await db.execute(select(PatchJob).where(PatchJob.id == job_id))).scalar_one_or_none()
    if job is None or job.device_id != device_id:
        return False

    installed = [str(i) for i in (data.get("installed") or [])]
    failed = data.get("failed") or {}
    job.installed = installed
    job.failed = failed if isinstance(failed, dict) else {}
    job.reboot_required = bool(data.get("reboot_required"))
    job.finished_at = datetime.now(UTC)
    if data.get("status") == "cancelled":
        job.status = PatchJobStatus.cancelled
    elif job.failed and installed:
        job.status = PatchJobStatus.partial
    elif job.failed:
        job.status = PatchJobStatus.failed
    else:
        job.status = PatchJobStatus.succeeded
    log.info(
        "patch job finished",
        job_id=str(job_id),
        status=job.status.value,
        installed=len(installed),
        failed=len(job.failed),
        reboot_required=job.reboot_required,
    )
    return True


async def apply_job_result(
    db: AsyncSession, device_id: uuid.UUID, job_id: uuid.UUID, data: dict
) -> None:
    run = await _run_for(db, job_id, device_id)
    if run is None:
        if await _apply_patch_job_result(db, device_id, job_id, data):
            return
        run = await _adopt_scheduled_run(db, device_id, job_id, data)
    if run is None:
        log.warning("result for unknown run", job_id=str(job_id))
        return
    status = data.get("status", "failed")
    try:
        run.status = RunStatus(status)
    except ValueError:
        run.status = RunStatus.failed
    run.exit_code = data.get("exit_code")
    run.truncated = run.truncated or bool(data.get("truncated"))
    run.finished_at = datetime.now(UTC)
    if run.started_at is None:
        run.started_at = run.finished_at
    await _apply_agent_version(db, device_id, job_id, data)
    log.info(
        "job finished",
        job_id=str(job_id),
        status=run.status.value,
        exit_code=run.exit_code,
    )


async def _adopt_scheduled_run(
    db: AsyncSession, device_id: uuid.UUID, job_id: uuid.UUID, data: dict
) -> ScriptRun | None:
    """Create the row for a scheduled run the server never queued.

    A scheduled fire comes out of the AGENT's cache, possibly while it was
    offline, so no server ever assigned it a job id or wrote a run row. The
    result therefore arrives for an id nothing knows about, and it used to be
    logged as an unknown run and dropped: a nightly job could run correctly on
    every machine in the fleet for a month and leave no record anywhere.

    The agent echoes `entry_id` back for exactly this, and the job id is a
    UUIDv5 over (entry, unjittered fire time). Recomputing it from the entry
    is what proves this result really belongs to that schedule rather than
    being an arbitrary id someone published.
    """
    entry_id = data.get("entry_id")
    if not entry_id:
        return None
    try:
        schedule_id = uuid.UUID(str(entry_id))
    except ValueError:
        log.warning("scheduled result with a malformed entry_id", entry_id=entry_id)
        return None

    schedule = await db.get(ScriptSchedule, schedule_id)
    if schedule is None:
        # The schedule was deleted while a fire was in flight. Worth saying:
        # the run really happened on a real machine.
        log.warning(
            "result for a schedule that no longer exists",
            entry_id=str(schedule_id),
            job_id=str(job_id),
        )
        return None

    run = ScriptRun(
        id=job_id,
        script_id=schedule.script_id,
        device_id=device_id,
        trigger=RunTrigger.schedule,
        status=RunStatus.running,
        requested_by=f"schedule:{schedule.name or schedule.id}",
    )
    db.add(run)
    await db.flush()
    schedule.last_run_at = datetime.now(UTC)
    log.info(
        "scheduled run adopted",
        job_id=str(job_id),
        entry_id=str(schedule_id),
        device_id=str(device_id),
    )
    return run


async def _apply_agent_version(
    db: AsyncSession, device_id: uuid.UUID, job_id: uuid.UUID, data: dict
) -> None:
    """Record a completed self-update against the device.

    `finalizing` is the whole reason this is a function and not a line. On
    Windows the swap is handed to a helper that runs after the agent exits, so
    a finalizing result means the binary on disk MIGHT change and the host is
    demonstrably still on the old version right now. Recording the new version
    there tells a ring rollout the host has moved when it has not, and the
    ring advances over a fleet that never updated. The agent's next heartbeat
    carries the truth either way, so waiting for it costs nothing.
    """
    updated_to = data.get("updated_to")
    if not updated_to or data.get("finalizing"):
        return
    if data.get("status") != "succeeded":
        return
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None or device.agent_version == updated_to:
        return
    log.info(
        "agent version updated",
        device_id=str(device_id),
        job_id=str(job_id),
        was=device.agent_version,
        now=updated_to,
    )
    device.agent_version = updated_to
