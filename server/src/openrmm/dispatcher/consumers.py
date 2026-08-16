"""JetStream durable consumers -> ingest handlers.

These are PUSH consumers, deliberately. An earlier pull-based version went
silently deaf: after a reconnect the durable's pull requests stopped being
answered, `fetch()` returned nothing forever, and ingest stopped while the
dispatcher still looked healthy (only a restart cleared it). Push delivery has
no polling to go stale, and a dead subscription surfaces as an exception
instead of silence.
"""

import asyncio

import nats
import nats.errors
import nats.js
import structlog
from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy

from openrmm.alerting.engine import AlertEngine
from openrmm.db.engine import session_scope
from openrmm.ingest.events import parse_agent_event, record_agent_event
from openrmm.ingest.inventory import apply_inventory, parse_inventory
from openrmm.ingest.results import (
    apply_job_output,
    apply_job_result,
    parse_job_output,
    parse_job_result,
)
from openrmm.ingest.telemetry import apply_telemetry, parse_telemetry

log = structlog.get_logger()

NAK_DELAY_S = 10

# One engine instance per dispatcher process: it holds the breach state
# machine and the rule cache.
ENGINE = AlertEngine()


async def _consume(
    js: nats.js.JetStreamContext,
    stream: str,
    durable: str,
    subject: str,
    handler,
    deliver: DeliverPolicy,
) -> None:
    sub = await js.subscribe(
        subject,
        stream=stream,
        durable=durable,
        manual_ack=True,
        config=ConsumerConfig(
            durable_name=durable,
            deliver_policy=deliver,
            ack_policy=AckPolicy.EXPLICIT,
            ack_wait=60,
            max_ack_pending=512,
        ),
    )
    log.info("consumer running", stream=stream, durable=durable)
    async for msg in sub.messages:
        try:
            await handler(msg.subject, msg.data)
            await msg.ack()
        except Exception:
            log.exception("ingest failed", subject=msg.subject)
            await msg.nak(delay=NAK_DELAY_S)


async def _supervised(coro_factory, name: str) -> None:
    """A consumer that dies must come back, and must say so."""
    while True:
        try:
            await coro_factory()
        except asyncio.CancelledError:
            raise
        except Exception:
            log.exception("consumer crashed, restarting", consumer=name)
            await asyncio.sleep(5)


async def _handle_telemetry(subject: str, data: bytes) -> None:
    parsed = parse_telemetry(subject, data)
    if parsed is None:
        log.warning("malformed telemetry", subject=subject)
        return
    device_id, ts, payload = parsed
    async with session_scope() as db:
        await apply_telemetry(db, device_id, ts, payload)
        # Evaluate in the same transaction: an alert and its outbox rows commit
        # together with the sample that caused them.
        await ENGINE.evaluate_telemetry(db, device_id, payload)


async def _handle_inventory(subject: str, data: bytes) -> None:
    parsed = parse_inventory(subject, data)
    if parsed is None:
        log.warning("malformed inventory", subject=subject)
        return
    device_id, kind, observed_at, payload = parsed
    async with session_scope() as db:
        await apply_inventory(db, device_id, kind, observed_at, payload)


async def _handle_job_output(subject: str, data: bytes) -> None:
    parsed = parse_job_output(subject, data)
    if parsed is None:
        log.warning("malformed job output", subject=subject)
        return
    device_id, job_id, payload = parsed
    async with session_scope() as db:
        await apply_job_output(db, device_id, job_id, payload)


async def _handle_job_result(subject: str, data: bytes) -> None:
    parsed = parse_job_result(subject, data)
    if parsed is None:
        log.warning("malformed job result", subject=subject)
        return
    device_id, job_id, payload = parsed
    async with session_scope() as db:
        await apply_job_result(db, device_id, job_id, payload)


async def _handle_event(subject: str, data: bytes) -> None:
    parsed = parse_agent_event(subject, data)
    if parsed is None:
        return
    device_id, payload = parsed
    async with session_scope() as db:
        await record_agent_event(db, device_id, payload)


# Telemetry history is already in the database and samples are cheap to miss
# for a moment; everything else replays its retained backlog (all ingest is
# idempotent, so a replay is safe).
SPECS = [
    ("TELEMETRY", "ing-telemetry", "agents.*.telemetry", _handle_telemetry, DeliverPolicy.NEW),
    ("INVENTORY", "ing-inventory", "agents.*.inventory.>", _handle_inventory, DeliverPolicy.ALL),
    ("JOBOUT", "ing-joboutput", "agents.*.jobs.*.output", _handle_job_output, DeliverPolicy.ALL),
    ("RESULTS", "ing-jobresults", "agents.*.jobs.*.result", _handle_job_result, DeliverPolicy.ALL),
    ("EVENTS", "ing-events", "agents.*.events", _handle_event, DeliverPolicy.ALL),
]


def start_consumers(js: nats.js.JetStreamContext) -> list[asyncio.Task]:
    tasks = []
    for stream, durable, subject, handler, deliver in SPECS:

        def factory(s=stream, d=durable, subj=subject, h=handler, dp=deliver):
            return _consume(js, s, d, subj, h, dp)

        tasks.append(asyncio.create_task(_supervised(factory, durable), name=f"{durable}-consumer"))
    return tasks
