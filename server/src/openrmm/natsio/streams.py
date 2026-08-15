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
]


async def ensure_streams(js: nats.js.JetStreamContext) -> None:
    for config in STREAMS:
        try:
            await js.add_stream(config)
            log.info("stream created", stream=config.name)
        except nats.js.errors.BadRequestError:
            await js.update_stream(config)
            log.info("stream updated", stream=config.name)
