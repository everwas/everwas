"""Bring already-existing JetStream durables up to the current config.

JetStream's CONSUMER.CREATE is bind-if-exists: passing a new `ConsumerConfig`
for a durable that already exists returns the OLD consumer, unchanged, with no
error. Every consumer setting we have added since a deployment first ran is
therefore inert on that deployment, and the code reads as though it is in
force. That is the whole failure class this module exists for.

Concretely: `max_deliver` was added to the ingest consumers after the fleet was
already running, and agents create their own job durables and prefer an
existing one over rewriting it. On any stack older than those changes a poison
message still redelivers forever, which is exactly the behaviour the setting
was added to stop.
"""

import nats
import nats.js
import structlog
from nats.js.api import ConsumerConfig
from nats.js.errors import NotFoundError

log = structlog.get_logger()

# Settings that JetStream lets you change on a live consumer. Anything else
# (deliver_policy, filter_subject, ack_policy) needs the consumer deleted and
# recreated, which loses its delivery position, so it is deliberately not done
# automatically.
MUTABLE = ("max_deliver", "ack_wait", "max_ack_pending")


def _drift(current: ConsumerConfig, wanted: dict) -> dict:
    """Which of the wanted values differ from what is live."""
    out = {}
    for field, want in wanted.items():
        have = getattr(current, field, None)
        # JetStream reports "unlimited" as -1 and nats-py sometimes as None.
        if have != want:
            out[field] = {"have": have, "want": want}
    return out


async def reconcile_consumer(
    js: nats.js.JetStreamContext, stream: str, durable: str, wanted: dict
) -> bool:
    """Update one durable's mutable settings. Returns True if it changed.

    A failure here is logged, not raised. Reconciliation is a repair, and a
    stack that cannot be repaired should still start and ingest: refusing to
    boot over a consumer setting turns a degraded system into an outage.
    """
    wanted = {k: v for k, v in wanted.items() if k in MUTABLE}
    try:
        info = await js.consumer_info(stream, durable)
    except NotFoundError:
        return False  # nothing to repair; it will be created with the right config
    except Exception as exc:
        log.warning("consumer info failed", stream=stream, durable=durable, err=str(exc))
        return False

    drift = _drift(info.config, wanted)
    if not drift:
        return False

    # Send the FULL existing config with the drifted fields overridden. Sending
    # only the changed keys resets every unspecified field to its default,
    # which would quietly widen ack_wait and max_ack_pending across the fleet.
    config = info.config
    for field, change in drift.items():
        setattr(config, field, change["want"])

    try:
        await js.add_consumer(stream, config=config)
    except Exception as exc:
        log.warning(
            "consumer reconcile failed",
            stream=stream,
            durable=durable,
            drift=drift,
            err=str(exc),
        )
        return False

    log.info("consumer reconciled", stream=stream, durable=durable, drift=drift)
    return True


async def reconcile_agent_job_consumers(
    js: nats.js.JetStreamContext, stream: str, wanted: dict
) -> int:
    """Repair every per-agent job durable. Returns how many changed.

    Agents create these themselves and deliberately bind to an existing one
    rather than rewriting it on each restart, so the agent can never fix an
    old durable no matter how many times it is upgraded. Only the server sees
    all of them, so only the server can do this.
    """
    try:
        consumers = await js.consumers_info(stream)
    except Exception as exc:
        log.warning("could not list consumers", stream=stream, err=str(exc))
        return 0

    repaired = 0
    for info in consumers:
        name = info.config.durable_name or info.name
        if not name or not name.startswith("agent-"):
            continue
        if await reconcile_consumer(js, stream, name, wanted):
            repaired += 1

    log.info(
        "agent job consumers reconciled",
        stream=stream,
        checked=len(consumers),
        repaired=repaired,
    )
    return repaired
