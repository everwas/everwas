import hashlib
import uuid
from datetime import datetime
from zoneinfo import ZoneInfo

import structlog
from croniter import croniter
from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import select

from everwas.api.deps import CurrentUser, DbSession, require_role
from everwas.dispatcher.schedule_sync import invalidate_schedule_cache
from everwas.models.device import Device
from everwas.models.script import RunStatus, Script, ScriptRun, ScriptSchedule
from everwas.models.user import Role
from everwas.natsio.client import get_nats
from everwas.schemas.script import (
    RunBatchOut,
    RunRequest,
    ScheduleIn,
    ScheduleOut,
    ScriptIn,
    ScriptOut,
    ScriptRunOut,
)
from everwas.services.jobs import (
    TargetError,
    cancel_run,
    device_matches_target,
    queue_script_run,
    resolve_targets,
)

router = APIRouter()
log = structlog.get_logger()

OPERATOR = require_role(Role.admin, Role.operator)


def _sha256(body: str) -> str:
    return hashlib.sha256(body.encode()).hexdigest()


@router.get("")
async def list_scripts(db: DbSession, _user: CurrentUser) -> list[ScriptOut]:
    rows = await db.execute(select(Script).order_by(Script.name))
    return [ScriptOut.model_validate(s) for s in rows.scalars()]


@router.post("", status_code=status.HTTP_201_CREATED, dependencies=[OPERATOR])
async def create_script(body: ScriptIn, db: DbSession, user: CurrentUser) -> ScriptOut:
    script = Script(
        **body.model_dump(),
        sha256=_sha256(body.body),
        updated_by=user.email,
    )
    db.add(script)
    await db.commit()
    return ScriptOut.model_validate(script)


@router.put("/{script_id}", dependencies=[OPERATOR])
async def update_script(
    script_id: uuid.UUID, body: ScriptIn, db: DbSession, user: CurrentUser
) -> ScriptOut:
    script = (await db.execute(select(Script).where(Script.id == script_id))).scalar_one_or_none()
    if script is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown script")
    for field, value in body.model_dump().items():
        setattr(script, field, value)
    script.sha256 = _sha256(body.body)
    script.version += 1
    script.updated_by = user.email
    await db.commit()
    return ScriptOut.model_validate(script)


@router.delete("/{script_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[OPERATOR])
async def delete_script(script_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> None:
    script = (await db.execute(select(Script).where(Script.id == script_id))).scalar_one_or_none()
    if script is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown script")
    await db.delete(script)
    await db.commit()


@router.post("/{script_id}/run", dependencies=[OPERATOR])
async def run_script(
    script_id: uuid.UUID, body: RunRequest, db: DbSession, user: CurrentUser
) -> RunBatchOut:
    script = (await db.execute(select(Script).where(Script.id == script_id))).scalar_one_or_none()
    if script is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown script")

    try:
        devices = await resolve_targets(
            db,
            {"device_ids": [str(d) for d in body.device_ids], "tags": body.tags, "all": body.all},
            script,
        )
    except TargetError as exc:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, str(exc)) from exc
    if not devices:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "no devices matched the target")

    # No NATS here on purpose: the runs and their outbox rows commit together
    # and the dispatcher delivers them. A broker outage delays a run, it does
    # not lose one, and it never runs one we have no record of.
    batch_id, runs = await queue_script_run(db, None, script, devices, requested_by=user.email)
    await db.commit()
    return RunBatchOut(batch_id=batch_id, queued=len(runs), run_ids=[r.id for r in runs])


@router.get("/runs/recent")
async def recent_runs(
    db: DbSession,
    _user: CurrentUser,
    limit: int = Query(default=50, ge=1, le=500),
    device_id: uuid.UUID | None = None,
) -> list[ScriptRunOut]:
    stmt = select(ScriptRun).order_by(ScriptRun.queued_at.desc()).limit(limit)
    if device_id is not None:
        stmt = stmt.where(ScriptRun.device_id == device_id)
    rows = await db.execute(stmt)
    return [ScriptRunOut.model_validate(r) for r in rows.scalars()]


@router.get("/runs/{run_id}")
async def get_run(run_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> ScriptRunOut:
    run = (await db.execute(select(ScriptRun).where(ScriptRun.id == run_id))).scalar_one_or_none()
    if run is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown run")
    return ScriptRunOut.model_validate(run)


@router.post("/runs/{run_id}/cancel", dependencies=[OPERATOR])
async def cancel(run_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> ScriptRunOut:
    run = (await db.execute(select(ScriptRun).where(ScriptRun.id == run_id))).scalar_one_or_none()
    if run is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown run")
    if run.status in (RunStatus.succeeded, RunStatus.failed, RunStatus.timeout):
        raise HTTPException(status.HTTP_409_CONFLICT, "run already finished")
    await cancel_run(db, get_nats(), run)
    await db.commit()
    return ScriptRunOut.model_validate(run)


# --------------------------------------------------------------------------
# Schedules
# --------------------------------------------------------------------------
#
# A schedule is not dispatched from here. It is pushed to the agents it
# targets, which fire it from their own cache on their own clock, so it runs
# on a machine that is off the network at 02:00. Nothing in this file talks to
# an agent: the dispatcher reconciles each device on its next heartbeat by
# comparing the version the agent reports against the one its entries hash to.


@router.get("/schedules")
async def list_schedules(db: DbSession, _user: CurrentUser) -> list[ScheduleOut]:
    rows = await db.execute(select(ScriptSchedule).order_by(ScriptSchedule.name))
    return [ScheduleOut.model_validate(s) for s in rows.scalars()]


@router.post("/schedules", status_code=status.HTTP_201_CREATED, dependencies=[OPERATOR])
async def create_schedule(body: ScheduleIn, db: DbSession, _user: CurrentUser) -> ScheduleOut:
    if (await db.get(Script, body.script_id)) is None:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "unknown script")
    schedule = ScriptSchedule(**body.model_dump())
    db.add(schedule)
    await db.commit()
    invalidate_schedule_cache()
    log.info("schedule created", schedule_id=str(schedule.id), cron=schedule.cron)
    return ScheduleOut.model_validate(schedule)


@router.put("/schedules/{schedule_id}", dependencies=[OPERATOR])
async def update_schedule(
    schedule_id: uuid.UUID, body: ScheduleIn, db: DbSession, _user: CurrentUser
) -> ScheduleOut:
    schedule = await db.get(ScriptSchedule, schedule_id)
    if schedule is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown schedule")
    if (await db.get(Script, body.script_id)) is None:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "unknown script")
    for field, value in body.model_dump().items():
        setattr(schedule, field, value)
    await db.commit()
    invalidate_schedule_cache()
    return ScheduleOut.model_validate(schedule)


@router.delete(
    "/schedules/{schedule_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[OPERATOR]
)
async def delete_schedule(schedule_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> None:
    schedule = await db.get(ScriptSchedule, schedule_id)
    if schedule is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown schedule")
    await db.delete(schedule)
    await db.commit()
    invalidate_schedule_cache()
    # The agents holding this entry find out on their next heartbeat: their
    # reported version no longer matches, so a document without it is pushed.
    log.info("schedule deleted", schedule_id=str(schedule_id))


@router.get("/schedules/{schedule_id}/preview")
async def preview_schedule(schedule_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> dict:
    """Which devices this schedule targets, and when it fires next.

    A cron expression plus a target selector is two things an operator cannot
    check by reading. Getting either wrong means a job that silently runs
    nowhere, or on everything.
    """
    schedule = await db.get(ScriptSchedule, schedule_id)
    if schedule is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown schedule")

    devices = (await db.execute(select(Device))).scalars().all()
    try:
        matched = [d for d in devices if device_matches_target(d, schedule.target or {})]
    except TargetError as exc:
        # A row saved before the target validator existed. The reconciler now
        # skips it silently for the fleet's sake, so this is where an operator
        # finds out why the schedule never fires, and the message says how to
        # fix it. A 500 here would read as "preview is broken".
        raise HTTPException(status.HTTP_400_BAD_REQUEST, str(exc)) from exc

    tz = ZoneInfo(schedule.tz or "UTC")
    itr = croniter(schedule.cron, datetime.now(tz))
    upcoming = [itr.get_next(datetime).isoformat() for _ in range(5)]

    return {
        "schedule_id": str(schedule_id),
        "matches": len(matched),
        "devices": [{"id": str(d.id), "hostname": d.hostname} for d in matched[:50]],
        "next_fires": upcoming,
        "jitter_s": schedule.jitter_s,
        "detail": (
            f"fires on {len(matched)} device(s); each spreads its start over "
            f"{schedule.jitter_s}s, deterministically per device"
        ),
    }
