from contextlib import asynccontextmanager

import structlog
from fastapi import FastAPI

from openrmm import __version__
from openrmm.api.v1 import agents, auth, devices, health, scripts, shell
from openrmm.db.engine import get_engine
from openrmm.natsio import client as nats_client

log = structlog.get_logger()


@asynccontextmanager
async def lifespan(app: FastAPI):
    get_engine()
    try:
        await nats_client.connect()
    except Exception as exc:  # the API still serves reads without NATS
        log.warning("api could not connect to nats", error=str(exc))
    yield
    await nats_client.close()
    await get_engine().dispose()


def create_app() -> FastAPI:
    app = FastAPI(
        title="OpenRMM",
        version=__version__,
        lifespan=lifespan,
        docs_url="/api/docs",
        openapi_url="/api/openapi.json",
    )
    app.include_router(health.router, prefix="/api/v1", tags=["health"])
    app.include_router(auth.router, prefix="/api/v1/auth", tags=["auth"])
    app.include_router(agents.router, prefix="/api/v1/agents", tags=["agents"])
    app.include_router(devices.router, prefix="/api/v1/devices", tags=["devices"])
    app.include_router(scripts.router, prefix="/api/v1/scripts", tags=["scripts"])
    app.include_router(shell.router, prefix="/api/v1/devices", tags=["shell"])
    return app


app = create_app()
