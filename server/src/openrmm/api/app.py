from contextlib import asynccontextmanager

from fastapi import FastAPI

from openrmm import __version__
from openrmm.api.v1 import auth, health
from openrmm.db.engine import get_engine


@asynccontextmanager
async def lifespan(app: FastAPI):
    get_engine()
    yield
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
    return app


app = create_app()
