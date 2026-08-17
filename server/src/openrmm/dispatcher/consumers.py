"""JetStream durable consumers -> ingest handlers.

These are PUSH consumers, deliberately. An earlier pull-based version went
silently deaf: after a reconnect the durable's pull requests stopped being
answered, `fetch()` returned nothing forever, and ingest stopped while the
dispatcher still looked healthy (only a restart cleared it). Push delivery has
no polling to go stale, and a dead subscription surfaces as an exception
instead of silence.
"""

import asyncio
import traceback
from datetime import UTC, datetime

import nats
import nats.errors
import nats.js
import structlog
from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy

from openrmm.alerting.engine import AlertEngine
from openrmm.bitemporal.store import WholesaleRetirementError
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
from openrmm.models.deadletter import IngestDeadLetter

log = structlog.get_logger()

NAK_DELAY_S = 10
MAX_DELIVER = 6
IDLE_HEARTBEAT_S = 30
RESTART_BACKOFF_S = 5

# One engine instance per dispatcher process: it holds the breach state
# machine and the rule cache.
ENGINE = AlertEngine()

# Last time each consumer successfully processed a message. /health/ingest
# reads this: silence is the alarm, so a consumer that has gone quiet while
# its stream has pending messages is reported unhealthy.
LAST_PROGRESS: dict[str, datetime] = {}


def _mark_progress(durable: str) -> None:
    LAST_PROGRESS[durable] = datetime.now(UTC)


def _delivery_count(msg) -> int:
    try:
        return int(msg.metadata.num_delivered or 1)
    except Exception:
        return 1


class PermanentIngestError(Exception):
    """A message that cannot be processed now and will not be later.

    Retrying a deterministic refusal wastes deliveries and buries the reason
    under six identical stack traces. Handlers raise this to say "park it and
    move on"; anything else is treated as possibly transient and retried.
    """


async def _dead_letter(stream: str, durable: str, msg, delivered: int) -> None:
    """Park a message that can never be processed, with why."""
    try:
        async with session_scope() as db:
            db.add(
                IngestDeadLetter(
                    stream=stream,
                    durable=durable,
                    subject=msg.subject,
                    payload=msg.data[:MAX_DEAD_LETTER_BYTES],
                    delivered=delivered,
                    error=traceback.format_exc()[:4000],
                )
            )
        log.error(
            "message dead-lettered after repeated failures",
            stream=stream,
            subject=msg.subject,
            delivered=delivered,
        )
    except Exception:
        # Never let the dead-letter write itself become the poison.
        log.exception("could not dead-letter message", subject=msg.subject)


MAX_DEAD_LETTER_BYTES = 64 * 1024


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
            # Without this a message that can NEVER be stored redelivers every
            # 10 seconds forever. One endpoint with a bad clock produced 12
            # failures in 30 seconds on a live stack and would not have
            # stopped. A poison message must give up and be recorded.
            max_deliver=MAX_DELIVER,
            # Surface a dead deliver subject instead of blocking forever on an
            # iterator nobody is publishing to.
            idle_heartbeat=IDLE_HEARTBEAT_S,
        ),
    )
    log.info("consumer running", stream=stream, durable=durable)
    async for msg in sub.messages:
        try:
            await handler(msg.subject, msg.data)
            await msg.ack()
            _mark_progress(durable)
        except PermanentIngestError:
            # Deterministically unprocessable: the same bytes will be refused
            # every time, so five more deliveries are five more identical
            # refusals. Park it now with the reason rather than after 60
            # seconds of retries and a stack trace that buries it.
            await _dead_letter(stream, durable, msg, _delivery_count(msg))
            await msg.ack()
        except Exception:
            delivered = _delivery_count(msg)
            if delivered >= MAX_DELIVER:
                # Last chance: park it where an operator can find it rather
                # than letting max_deliver drop it silently.
                await _dead_letter(stream, durable, msg, delivered)
                await msg.ack()
            else:
                log.exception(
                    "ingest failed", subject=msg.subject, delivered=delivered, stream=stream
                )
                await msg.nak(delay=NAK_DELAY_S)


async def _supervised(coro_factory, name: str) -> None:
    """A task that dies must come back, and must say so.

    Note the backoff is outside the except: a coroutine that RETURNS (rather
    than raising) would otherwise be restarted in a tight loop at full CPU.
    Returning is itself an anomaly here, since every one of these is meant to
    run until cancelled, so it is logged.
    """
    while True:
        try:
            await coro_factory()
            log.warning("task returned unexpectedly, restarting", task=name)
        except asyncio.CancelledError:
            raise
        except Exception:
            log.exception("task crashed, restarting", task=name)
        await asyncio.sleep(RESTART_BACKOFF_S)


def supervise(coro_factory, name: str) -> asyncio.Task:
    """Public wrapper so the dispatcher can supervise non-consumer tasks too."""
    return asyncio.create_task(_supervised(coro_factory, name), name=name)


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
        await ENGINE.evaluate_telemetry(db, device_id, payload, ts)


async def _handle_inventory(subject: str, data: bytes) -> None:
    parsed = parse_inventory(subject, data)
    if parsed is None:
        log.warning("malformed inventory", subject=subject)
        return
    device_id, kind, observed_at, payload = parsed
    try:
        async with session_scope() as db:
            await apply_inventory(db, device_id, kind, observed_at, payload)
    except WholesaleRetirementError as exc:
        # A snapshot asserting the device has none of a kind, when we currently
        # believe it has some. Never retryable: the payload is fixed.
        log.error("refused inventory snapshot", subject=subject, kind=kind, reason=str(exc))
        raise PermanentIngestError(str(exc)) from exc


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


async def reconcile_durables(js: nats.js.JetStreamContext) -> None:
    """Repair consumers created before the current settings existed.

    JetStream binds to an existing durable rather than reconfiguring it, so a
    stack that first ran before `max_deliver` was added still redelivers poison
    messages forever, with code in front of it that says otherwise.
    """
    from openrmm.dispatcher.reconcile import reconcile_agent_job_consumers, reconcile_consumer

    wanted = {"max_deliver": MAX_DELIVER, "ack_wait": 60, "max_ack_pending": 512}
    for stream, durable, _subject, _handler, _deliver in SPECS:
        await reconcile_consumer(js, stream, durable, wanted)

    # Per-agent job durables are created BY the agent, which binds to an
    # existing one on every restart, so upgrading the agent never fixes an old
    # durable. These values mirror internal/jobs/consumer.go.
    await reconcile_agent_job_consumers(
        js, "JOBS", {"max_deliver": 3, "ack_wait": 30, "max_ack_pending": 16}
    )


def start_consumers(js: nats.js.JetStreamContext) -> list[asyncio.Task]:
    tasks = []
    for stream, durable, subject, handler, deliver in SPECS:

        def factory(s=stream, d=durable, subj=subject, h=handler, dp=deliver):
            return _consume(js, s, d, subj, h, dp)

        tasks.append(asyncio.create_task(_supervised(factory, durable), name=f"{durable}-consumer"))
    return tasks
