"""Job dispatch: queue work to agents through the durable JOBS stream.

Jobs survive agent downtime — a script queued to an offline laptop runs when
it reconnects (JetStream 7d retention, per-agent durable consumer).

Queueing does not publish. The run row, its outbox row, and the audit entry
commit together; the dispatcher's job-outbox drainer publishes afterwards. An
earlier version published inside the request, before the caller committed, so
a mid-batch NATS failure left scripts running as root on N machines with no
run row, no output, and no audit trail.
"""

import json
import time
import uuid
from datetime import UTC, datetime

import nats
import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.config import get_settings
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.job_outbox import KIND_SCRIPT_RUN, JobOutbox, JobOutboxStatus
from openrmm.models.script import RunStatus, RunTrigger, Script, ScriptRun
from openrmm.natsio.subjects import jobs_queue
from openrmm.util.ids import uuid7

log = structlog.get_logger()


class TargetError(ValueError):
    """The caller's target selector cannot be honoured. Maps to HTTP 400."""


class AmbiguousTarget(TargetError):
    """More than one selector was given, so the blast radius is not obvious."""


class TooManyTargets(TargetError):
    """The selector matches more devices than one run is allowed to touch."""


def job_envelope(agent_id: str, job_id: str, kind: str, payload: dict) -> bytes:
    return json.dumps(
        {
            "v": 1,
            "type": "job",
            "agent_id": agent_id,
            "msg_id": uuid.uuid4().hex,
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "data": {"job_id": job_id, "kind": kind, **payload},
        }
    ).encode()


def _selector(target: dict) -> tuple[str, list] | None:
    """Exactly one of device_ids / tags / all, or nothing at all.

    Ambiguity is an error, not a preference order. Callers routinely send all
    three keys (the API always does), so an if/elif chain silently ran
    {device_ids: [...], all: true} against the device list only.
    """
    chosen: list[tuple[str, list]] = []
    if device_ids := target.get("device_ids"):
        chosen.append(("device_ids", list(device_ids)))
    if tags := target.get("tags"):
        chosen.append(("tags", list(tags)))
    if target.get("all"):
        chosen.append(("all", []))

    if len(chosen) > 1:
        named = ", ".join(name for name, _ in chosen)
        raise AmbiguousTarget(
            f"target names more than one selector ({named}); pass exactly one of "
            "device_ids, tags, or all so the blast radius is unambiguous"
        )
    return chosen[0] if chosen else None


def device_matches_target(device: Device, target: dict) -> bool:
    """Does this one device fall inside the selector? No query, no ceiling.

    For membership questions ("does this policy cover the device that just
    reported?") where materialising the whole fleet would be absurd.
    """
    if device.status is DeviceStatus.retired:
        return False
    selector = _selector(target or {})
    if selector is None:
        return False
    kind, value = selector
    if kind == "all":
        return True
    if kind == "device_ids":
        return str(device.id) in {str(d) for d in value}
    return bool(set(device.tags or []) & {str(t) for t in value})


async def resolve_targets(
    db: AsyncSession,
    target: dict,
    script: Script | None = None,
    *,
    max_targets: int | None = None,
) -> list[Device]:
    """Target selector: {device_ids: [...]} | {tags: [...]} | {all: true}.

    Raises AmbiguousTarget when more than one selector is given, and
    TooManyTargets above the configured ceiling. The ceiling is applied after
    the script's os_filter, because that is the number of machines that would
    actually be touched.
    """
    selector = _selector(target or {})
    if selector is None:
        return []
    kind, value = selector

    stmt = select(Device).where(Device.status != DeviceStatus.retired)
    if kind == "device_ids":
        try:
            ids = [uuid.UUID(str(d)) for d in value]
        except (ValueError, TypeError) as exc:
            raise TargetError(f"device_ids holds something that is not a UUID: {exc}") from exc
        stmt = stmt.where(Device.id.in_(ids))
    elif kind == "tags":
        stmt = stmt.where(Device.tags.overlap([str(t) for t in value]))

    devices = list((await db.execute(stmt)).scalars())
    if script and script.os_filter:
        devices = [d for d in devices if d.os_family.value in script.os_filter]

    ceiling = get_settings().max_run_targets if max_targets is None else max_targets
    if len(devices) > ceiling:
        raise TooManyTargets(
            f"that target matches {len(devices)} devices, above the {ceiling} device "
            "ceiling for a single run; narrow the selector, or raise "
            "OPENRMM_MAX_RUN_TARGETS if you mean it"
        )
    return devices


async def queue_script_run(
    db: AsyncSession,
    nc: nats.NATS | None,
    script: Script,
    devices: list[Device],
    *,
    requested_by: str,
    trigger: RunTrigger = RunTrigger.manual,
) -> tuple[uuid.UUID, list[ScriptRun]]:
    """Record run rows and their outbox rows. Returns (batch_id, runs).

    Publishes nothing: delivery happens after the caller commits, from the
    dispatcher. `nc` is accepted for call-site compatibility and unused —
    queueing a run no longer needs a live NATS connection.
    """
    batch_id = uuid7()
    runs: list[ScriptRun] = []

    for device in devices:
        run = ScriptRun(
            id=uuid7(),
            script_id=script.id,
            device_id=device.id,
            run_batch_id=batch_id,
            trigger=trigger,
            status=RunStatus.queued,
            requested_by=requested_by,
        )
        db.add(run)
        db.add(
            JobOutbox(
                id=run.id,  # the run id IS the wire job id, and the dedup key
                device_id=device.id,
                subject=jobs_queue(str(device.id)),
                kind=KIND_SCRIPT_RUN,
                payload={
                    "shell": script.shell.value,
                    "body": script.body,
                    "timeout_s": script.timeout_s,
                    "sha256": script.sha256,
                    "requested_by": requested_by,
                    "env": {},
                },
            )
        )
        runs.append(run)

    db.add(
        AuditLog(
            actor_type=ActorType.user,
            actor_id=requested_by,
            action="script.queued",
            target_type="script",
            target_id=str(script.id),
            detail={"batch_id": str(batch_id), "devices": len(devices), "name": script.name},
        )
    )
    await db.flush()
    log.info("script queued", script=script.name, devices=len(devices), batch_id=str(batch_id))
    return batch_id, runs


async def cancel_run(db: AsyncSession, nc: nats.NATS, run: ScriptRun) -> None:
    from openrmm.natsio.agent_request import request_agent

    # Undelivered work is cancelled by never sending it. Do this first: if the
    # row is still pending, the agent has nothing to cancel.
    outbox = (
        await db.execute(select(JobOutbox).where(JobOutbox.id == run.id))
    ).scalar_one_or_none()
    if outbox is not None and outbox.status is JobOutboxStatus.pending:
        outbox.status = JobOutboxStatus.cancelled
        outbox.last_error = "cancelled before dispatch"
        log.info("run cancelled before dispatch", run_id=str(run.id))
    else:
        try:
            await request_agent(
                nc,
                str(run.device_id),
                "job.cancel",
                json.dumps({"job_id": str(run.id)}).encode(),
                timeout=5,
            )
        except Exception:
            log.warning("cancel request failed (agent offline?)", run_id=str(run.id))
    run.status = RunStatus.cancelled
    run.finished_at = datetime.now(UTC)
