"""Idempotent JetStream stream provisioning. The server owns all streams."""

import nats.js
import structlog
from nats.js.api import DiscardPolicy, RetentionPolicy, StorageType, StreamConfig

log = structlog.get_logger()

STREAMS = [
    StreamConfig(
        name="TELEMETRY",
        subjects=["agents.*.telemetry"],
        retention=RetentionPolicy.LIMITS,
        max_age=48 * 3600,
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
    ),
    StreamConfig(
        name="INVENTORY",
        subjects=["agents.*.inventory.>"],
        retention=RetentionPolicy.LIMITS,
        max_msgs_per_subject=1,  # acts as a KV of the latest snapshot per kind
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
    ),
    # Durable job delivery: a script queued to an offline laptop runs when it
    # comes back. Per-agent durable pull consumers are created by the agents.
    StreamConfig(
        name="JOBS",
        subjects=["jobs.*"],
        retention=RetentionPolicy.LIMITS,
        max_age=7 * 24 * 3600,
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
        # Job dispatch is at-least-once by design: the outbox may republish a
        # row whose publish succeeded but whose bookkeeping did not. The
        # default 2-minute dedup window is shorter than a dispatcher restart,
        # so widen it. Nats-Msg-Id is the run/job id, which never changes.
        duplicate_window=2 * 3600,
    ),
    StreamConfig(
        name="JOBOUT",
        subjects=["agents.*.jobs.*.output"],
        retention=RetentionPolicy.LIMITS,
        max_age=24 * 3600,
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
    ),
    StreamConfig(
        name="RESULTS",
        subjects=["agents.*.jobs.*.result"],
        retention=RetentionPolicy.LIMITS,
        max_age=7 * 24 * 3600,
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
    ),
    StreamConfig(
        name="EVENTS",
        subjects=["agents.*.events"],
        retention=RetentionPolicy.LIMITS,
        max_age=90 * 24 * 3600,
        storage=StorageType.FILE,
        discard=DiscardPolicy.OLD,
    ),
]


async def ensure_streams(js: nats.js.JetStreamContext) -> None:
    for config in STREAMS:
        try:
            await js.add_stream(config)
            log.info("stream created", stream=config.name)
        except nats.js.errors.BadRequestError:
            await js.update_stream(config)
            log.info("stream updated", stream=config.name)
