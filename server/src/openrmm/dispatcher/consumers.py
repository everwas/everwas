"""JetStream durable pull consumers -> ingest handlers."""

import asyncio

import nats
import nats.errors
import nats.js
import structlog

from openrmm.db.engine import session_scope
from openrmm.ingest.inventory import apply_inventory, parse_inventory
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


def start_consumers(js: nats.js.JetStreamContext) -> list[asyncio.Task]:
    return [
        asyncio.create_task(
            _pull_loop(js, "TELEMETRY", "ingest-telemetry", _handle_telemetry),
            name="telemetry-consumer",
        ),
        asyncio.create_task(
            _pull_loop(js, "INVENTORY", "ingest-inventory", _handle_inventory),
            name="inventory-consumer",
        ),
    ]
