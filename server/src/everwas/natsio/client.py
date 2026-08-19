"""NATS connection for the API process (interactive request/reply + job publish)."""

import asyncio

import nats
import structlog

from everwas.config import get_settings

log = structlog.get_logger()

_nc: nats.NATS | None = None


async def connect() -> nats.NATS:
    global _nc
    settings = get_settings()
    # max_reconnect_attempts=-1 keeps a LIVE connection healing forever, but
    # nats-py also applies it to the INITIAL connect, which then retries
    # unboundedly instead of raising. The API's lifespan catches a failed
    # connect and serves reads without NATS; it can only do that if the
    # failure actually surfaces, so the first connect gets a hard deadline.
    _nc = await asyncio.wait_for(
        nats.connect(
            settings.nats_url,
            name="everwas-api",
            user=settings.nats_server_user,
            password=settings.nats_server_password,
            max_reconnect_attempts=-1,
        ),
        timeout=15,
    )
    log.info("api connected to nats")
    return _nc


async def close() -> None:
    global _nc
    if _nc is not None:
        await _nc.drain()
        _nc = None


def get_nats() -> nats.NATS:
    if _nc is None:
        raise RuntimeError("NATS connection not initialised")
    return _nc
