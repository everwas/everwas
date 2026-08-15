"""NATS connection for the API process (interactive request/reply + job publish)."""

import nats
import structlog

from openrmm.config import get_settings

log = structlog.get_logger()

_nc: nats.NATS | None = None


async def connect() -> nats.NATS:
    global _nc
    settings = get_settings()
    _nc = await nats.connect(
        settings.nats_url,
        name="openrmm-api",
        user=settings.nats_server_user,
        password=settings.nats_server_password,
        max_reconnect_attempts=-1,
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
