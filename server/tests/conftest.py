"""Integration fixtures: a scratch database on a real PostgreSQL server.

Uses OPENRMM_TEST_PG (base URL without database) or the dev compose instance.
Tests requiring it skip cleanly when no server is reachable.
"""

import os
import uuid

import pytest
from sqlalchemy.ext.asyncio import create_async_engine

TEST_PG = os.environ.get(
    "OPENRMM_TEST_PG",
    "postgresql+asyncpg://openrmm:{pw}@localhost:25432",
)


def _admin_url() -> str | None:
    url = TEST_PG
    if "{pw}" in url:
        pw = _dev_env_password()
        if pw is None:
            return None
        url = url.format(pw=pw)
    return url


def _dev_env_password() -> str | None:
    env_file = os.path.join(os.path.dirname(__file__), "..", "..", ".env")
    try:
        with open(env_file) as f:
            for line in f:
                if line.startswith("POSTGRES_PASSWORD="):
                    return line.strip().split("=", 1)[1]
    except OSError:
        return None
    return None


@pytest.fixture
async def pg_database():
    """Create a scratch database, run migrations, yield its URL, drop it."""
    admin_url = _admin_url()
    if admin_url is None:
        pytest.skip("no test PostgreSQL configured")

    dbname = f"openrmm_test_{uuid.uuid4().hex[:12]}"
    admin = create_async_engine(admin_url + "/postgres", isolation_level="AUTOCOMMIT")
    try:
        async with admin.connect() as conn:
            from sqlalchemy import text

            await conn.execute(text(f'CREATE DATABASE "{dbname}"'))
    except Exception:
        await admin.dispose()
        pytest.skip("test PostgreSQL unreachable")

    db_url = f"{admin_url}/{dbname}"
    os.environ["OPENRMM_DATABASE_URL"] = db_url

    # run migrations against the scratch db (settings cache must not hold the old URL)
    from openrmm.config import get_settings

    get_settings.cache_clear()

    from alembic.config import Config

    from alembic import command

    alembic_cfg = Config(os.path.join(os.path.dirname(__file__), "..", "alembic.ini"))
    await _run_sync(command.upgrade, alembic_cfg, "head")

    yield db_url

    from openrmm.db import engine as engine_mod

    if engine_mod._engine is not None:
        await engine_mod._engine.dispose()
        engine_mod._engine = None
        engine_mod._sessionmaker = None
    async with admin.connect() as conn:
        from sqlalchemy import text

        await conn.execute(text(f'DROP DATABASE "{dbname}" WITH (FORCE)'))
    await admin.dispose()
    get_settings.cache_clear()


async def _run_sync(fn, *args):
    import asyncio

    await asyncio.get_event_loop().run_in_executor(None, fn, *args)
