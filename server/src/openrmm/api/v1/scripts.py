import hashlib
import uuid

from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession, require_role
from openrmm.models.script import RunStatus, Script, ScriptRun
from openrmm.models.user import Role
from openrmm.natsio.client import get_nats
from openrmm.schemas.script import (
    RunBatchOut,
    RunRequest,
    ScriptIn,
    ScriptOut,
    ScriptRunOut,
)
from openrmm.services.jobs import (
    TargetError,
    cancel_run,
    queue_script_run,
    resolve_targets,
)

router = APIRouter()

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
