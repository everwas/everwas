"""Dispatcher process: JetStream consumers, alerting, schedulers, auth-callout.

M0: connects to NATS and idles. M1 adds the auth-callout responder and
heartbeat consumer.
"""

import asyncio
import signal

import nats
import structlog

from openrmm import __version__
from openrmm.config import get_settings

log = structlog.get_logger()


async def run() -> None:
    settings = get_settings()
    log.info("dispatcher starting", version=__version__, nats_url=settings.nats_url)

    nc = await nats.connect(
        settings.nats_url,
        name="openrmm-dispatcher",
        max_reconnect_attempts=-1,
    )
    log.info("dispatcher connected", server=nc.connected_url.netloc if nc.connected_url else None)

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    await stop.wait()
    log.info("dispatcher draining")
    await nc.drain()


def main() -> None:
    asyncio.run(run())
