"""Dispatcher process: ingest consumers, offline sweep, NATS auth-callout responder.

M1 scope: heartbeat consumer (core NATS), offline sweep, auth callout.
JetStream durable consumers arrive with telemetry in M2.
"""

import asyncio
import signal

import nats
import structlog

from everwas import __version__
from everwas.config import Settings, get_settings
from everwas.db.engine import session_scope
from everwas.dispatcher.consumers import ENGINE
from everwas.ingest.heartbeat import (
    apply_heartbeat,
    offline_devices,
    parse_heartbeat,
    sweep_offline,
)

log = structlog.get_logger()

SWEEP_INTERVAL_S = 30


async def heartbeat_consumer(nc: nats.NATS) -> None:
    from everwas.dispatcher.schedule_sync import ScheduleSyncer, set_syncer

    syncer = ScheduleSyncer(nc)
    set_syncer(syncer)
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
                device = await apply_heartbeat(db, agent_id, data)
                if device is None:
                    log.warning("heartbeat from unknown device", agent_id=str(agent_id))
                else:
                    # Idempotent: a no-op unless a heartbeat alert is open.
                    await ENGINE.resolve_for_device(db, device)
        except Exception:
            log.exception("heartbeat apply failed", agent_id=str(agent_id))
            continue

        if device is None:
            continue
        # Outside the session on purpose: this makes a NATS round trip to the
        # agent, and holding a database connection across it would tie up the
        # pool for the whole fleet on one slow machine. Reading device
        # attributes out here is only safe because the sessionmaker sets
        # expire_on_commit=False; with the default, target matching would hit
        # a lazy refresh on a closed session.
        try:
            await syncer.check(device, data.get("schedule_version"))
        except Exception:
            log.exception("schedule reconcile failed", agent_id=str(agent_id))


async def offline_sweep(settings: Settings) -> None:
    while True:
        await asyncio.sleep(SWEEP_INTERVAL_S)
        try:
            async with session_scope() as db:
                gone = await sweep_offline(db, settings.heartbeat_offline_after_s)
                # Evaluate EVERY offline device, not just the new transitions.
                # An alert attempt that was suppressed once must get another
                # chance, or the device sits offline forever with nothing on
                # the alert page.
                await ENGINE.evaluate_heartbeat_missed(db, await offline_devices(db))
            if gone:
                log.info("devices marked offline", count=len(gone))
        except Exception:
            log.exception("offline sweep failed")


async def partition_maintenance(settings: Settings) -> None:
    from everwas.services.partitions import ensure_partitions

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
        name="everwas-dispatcher",
        user=settings.nats_server_user,
        password=settings.nats_server_password,
        max_reconnect_attempts=-1,
    )
    log.info("dispatcher connected", server=nc.connected_url.netloc if nc.connected_url else None)

    # partitions must exist before ingest starts
    from everwas.services.partitions import ensure_partitions

    async with session_scope() as db:
        await ensure_partitions(db, settings.telemetry_retention_days)

    js = nc.jetstream()
    from everwas.natsio.streams import ensure_streams

    await ensure_streams(js)

    from everwas.dispatcher.consumers import reconcile_durables, start_consumers, supervise

    # Before anything subscribes: a durable that already exists keeps its old
    # config, so settings added since this stack first ran are inert until
    # they are pushed explicitly.
    await reconcile_durables(js)

    from everwas.services.job_outbox import job_outbox_loop
    from everwas.services.outbox import outbox_loop

    # EVERY long-lived task is supervised. These were previously bare
    # create_task calls that nobody awaited, so an exception left the process
    # running and apparently healthy with that function silently gone: no
    # heartbeats recorded, or the entire fleet swept offline, or notifications
    # never delivered, with a green /health the whole time.
    tasks = [
        supervise(lambda: heartbeat_consumer(nc), "heartbeat"),
        supervise(lambda: offline_sweep(settings), "sweep"),
        supervise(lambda: partition_maintenance(settings), "partitions"),
        supervise(outbox_loop, "outbox"),
        # Jobs are queued by the API and published here, after their rows have
        # committed. Nothing else publishes to JOBS.
        supervise(lambda: job_outbox_loop(js), "job-outbox"),
        *start_consumers(js),
    ]

    if settings.nats_auth_seed:
        from everwas.natsio.auth_callout import auth_callout_responder

        tasks.append(asyncio.create_task(auth_callout_responder(nc, settings), name="auth-callout"))
    else:
        log.warning("EVERWAS_NATS_AUTH_SEED unset — auth callout disabled (dev only)")

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
