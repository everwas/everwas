"""JetStream durable pull consumers -> ingest handlers."""

import asyncio

import nats
import nats.errors
import nats.js
import structlog

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

FETCH_BATCH = 64
FETCH_TIMEOUT_S = 5


async def _pull_loop(js: nats.js.JetStreamContext, stream: str, durable: str, handler) -> None:
    sub = await js.pull_subscribe("", durable=durable, stream=stream)
    log.info("consumer running", stream=stream, durable=durable)
    while True:
        try:
            msgs = await sub.fetch(FETCH_BATCH, timeout=FETCH_TIMEOUT_S)
        except nats.errors.TimeoutError:
            continue
        for msg in msgs:
            try:
                await handler(msg.subject, msg.data)
                await msg.ack()
            except Exception:
                log.exception("ingest failed", subject=msg.subject)
                await msg.nak(delay=10)


async def _handle_telemetry(subject: str, data: bytes) -> None:
    parsed = parse_telemetry(subject, data)
    if parsed is None:
        log.warning("malformed telemetry", subject=subject)
        return
    device_id, ts, payload = parsed
    async with session_scope() as db:
        await apply_telemetry(db, device_id, ts, payload)


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


def start_consumers(js: nats.js.JetStreamContext) -> list[asyncio.Task]:
    specs = [
        ("TELEMETRY", "ingest-telemetry", _handle_telemetry),
        ("INVENTORY", "ingest-inventory", _handle_inventory),
        ("JOBOUT", "ingest-joboutput", _handle_job_output),
        ("RESULTS", "ingest-jobresults", _handle_job_result),
        ("EVENTS", "ingest-events", _handle_event),
    ]
    return [
        asyncio.create_task(_pull_loop(js, stream, durable, handler), name=f"{durable}-consumer")
        for stream, durable, handler in specs
    ]
