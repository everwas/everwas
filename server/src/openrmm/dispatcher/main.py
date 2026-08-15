"""Dispatcher process: ingest consumers, offline sweep, NATS auth-callout responder.

M1 scope: heartbeat consumer (core NATS), offline sweep, auth callout.
JetStream durable consumers arrive with telemetry in M2.
"""

import asyncio
import signal

import nats
import structlog

from openrmm import __version__
from openrmm.config import Settings, get_settings
from openrmm.db.engine import session_scope
from openrmm.ingest.heartbeat import apply_heartbeat, parse_heartbeat, sweep_offline

log = structlog.get_logger()

SWEEP_INTERVAL_S = 30


async def heartbeat_consumer(nc: nats.NATS) -> None:
    sub = await nc.subscribe("agents.*.heartbeat", queue="dispatcher")
    log.info("heartbeat consumer subscribed")
    async for msg in sub.messages:
        parsed = parse_heartbeat(msg.subject, msg.data)
        if parsed is None:
            log.warning("malformed heartbeat", subject=msg.subject)
            continue
        agent_id, data = parsed
        try:
            async with session_scope() as db:
                known = await apply_heartbeat(db, agent_id, data)
            if not known:
                log.warning("heartbeat from unknown device", agent_id=str(agent_id))
        except Exception:
            log.exception("heartbeat apply failed", agent_id=str(agent_id))


async def offline_sweep(settings: Settings) -> None:
    while True:
        await asyncio.sleep(SWEEP_INTERVAL_S)
        try:
            async with session_scope() as db:
                flipped = await sweep_offline(db, settings.heartbeat_offline_after_s)
            if flipped:
                log.info("devices marked offline", count=flipped)
        except Exception:
            log.exception("offline sweep failed")


async def partition_maintenance(settings: Settings) -> None:
    from openrmm.services.partitions import ensure_partitions

    while True:
        try:
            async with session_scope() as db:
                await ensure_partitions(db, settings.telemetry_retention_days)
            log.info("partitions ensured")
        except Exception:
            log.exception("partition maintenance failed")
        await asyncio.sleep(24 * 3600)


async def run() -> None:
    settings = get_settings()
    log.info("dispatcher starting", version=__version__, nats_url=settings.nats_url)

    nc = await nats.connect(
        settings.nats_url,
        name="openrmm-dispatcher",
        user=settings.nats_server_user,
        password=settings.nats_server_password,
        max_reconnect_attempts=-1,
    )
    log.info("dispatcher connected", server=nc.connected_url.netloc if nc.connected_url else None)

    # partitions must exist before ingest starts
    from openrmm.services.partitions import ensure_partitions

    async with session_scope() as db:
        await ensure_partitions(db, settings.telemetry_retention_days)

    js = nc.jetstream()
    from openrmm.natsio.streams import ensure_streams

    await ensure_streams(js)

    from openrmm.dispatcher.consumers import start_consumers

    tasks = [
        asyncio.create_task(heartbeat_consumer(nc), name="heartbeat"),
        asyncio.create_task(offline_sweep(settings), name="sweep"),
        asyncio.create_task(partition_maintenance(settings), name="partitions"),
        *start_consumers(js),
    ]

    if settings.nats_auth_seed:
        from openrmm.natsio.auth_callout import auth_callout_responder

        tasks.append(asyncio.create_task(auth_callout_responder(nc, settings), name="auth-callout"))
    else:
        log.warning("OPENRMM_NATS_AUTH_SEED unset — auth callout disabled (dev only)")

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    await stop.wait()
    log.info("dispatcher draining")
    for task in tasks:
        task.cancel()
    await nc.drain()


def main() -> None:
    asyncio.run(run())
