"""Job dispatch: queue work to agents through the durable JOBS stream.

Jobs survive agent downtime — a script queued to an offline laptop runs when
it reconnects (JetStream 7d retention, per-agent durable consumer).
"""

import json
import time
import uuid
from datetime import UTC, datetime

import nats
import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.script import RunStatus, RunTrigger, Script, ScriptRun
from openrmm.natsio.subjects import jobs_queue
from openrmm.util.ids import uuid7

log = structlog.get_logger()


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


async def resolve_targets(
    db: AsyncSession, target: dict, script: Script | None = None
) -> list[Device]:
    """Target selector: {device_ids: [...]} | {tags: [...]} | {all: true}."""
    stmt = select(Device).where(Device.status != DeviceStatus.retired)
    if device_ids := target.get("device_ids"):
        stmt = stmt.where(Device.id.in_([uuid.UUID(str(d)) for d in device_ids]))
    elif tags := target.get("tags"):
        stmt = stmt.where(Device.tags.overlap(tags))
    elif not target.get("all"):
        return []
    devices = list((await db.execute(stmt)).scalars())
    if script and script.os_filter:
        devices = [d for d in devices if d.os_family.value in script.os_filter]
    return devices


async def queue_script_run(
    db: AsyncSession,
    nc: nats.NATS,
    script: Script,
    devices: list[Device],
    *,
    requested_by: str,
    trigger: RunTrigger = RunTrigger.manual,
) -> tuple[uuid.UUID, list[ScriptRun]]:
    """Create run rows and publish jobs. Returns (batch_id, runs)."""
    js = nc.jetstream()
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
        runs.append(run)
    await db.flush()

    for run, device in zip(runs, devices, strict=True):
        await js.publish(
            jobs_queue(str(device.id)),
            job_envelope(
                str(device.id),
                str(run.id),
                "script.run",
                {
                    "shell": script.shell.value,
                    "body": script.body,
                    "timeout_s": script.timeout_s,
                    "sha256": script.sha256,
                    "requested_by": requested_by,
                    "env": {},
                },
            ),
            headers={"Nats-Msg-Id": str(run.id)},  # dedup on redelivery
        )

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
    log.info("script queued", script=script.name, devices=len(devices), batch_id=str(batch_id))
    return batch_id, runs


async def cancel_run(db: AsyncSession, nc: nats.NATS, run: ScriptRun) -> None:
    from openrmm.natsio.subjects import cmd

    try:
        await nc.request(
            cmd(str(run.device_id), "job.cancel"),
            json.dumps({"job_id": str(run.id)}).encode(),
            timeout=5,
        )
    except Exception:
        log.warning("cancel request failed (agent offline?)", run_id=str(run.id))
    run.status = RunStatus.cancelled
    run.finished_at = datetime.now(UTC)
