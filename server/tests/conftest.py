"""Integration fixtures: a scratch database on a real PostgreSQL server.

Uses OPENRMM_TEST_PG (base URL without database) or the dev compose instance.
Tests requiring it skip cleanly when no server is reachable, so `pytest` works
on a laptop with nothing running.

Set OPENRMM_REQUIRE_PG=1 where the database is supposed to exist (CI) and a
missing one FAILS instead of skipping. Without that switch the whole suite goes
green while quietly testing almost nothing: for months CI ran 87 pure-function
tests and skipped 92, including every bitemporal, revocation, schedule and
deletion test. A skip is invisible in a green check mark, which makes silent
coverage loss the default failure mode rather than an unlikely one.
"""

import os
import uuid

import pytest
from sqlalchemy.ext.asyncio import create_async_engine

TEST_PG = os.environ.get(
    "OPENRMM_TEST_PG",
    "postgresql+asyncpg://openrmm:{pw}@localhost:25432",
)


def _required() -> bool:
    return os.environ.get("OPENRMM_REQUIRE_PG", "").lower() in {"1", "true", "yes"}


def _unavailable(reason: str) -> None:
    """Skip, or fail if this environment promised a database."""
    if _required():
        pytest.fail(
            f"OPENRMM_REQUIRE_PG is set but PostgreSQL is unusable: {reason}. "
            "Refusing to skip: silently dropping half the suite is what this flag exists "
            "to prevent."
        )
    pytest.skip(reason)


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
        _unavailable("no test PostgreSQL configured")

    dbname = f"openrmm_test_{uuid.uuid4().hex[:12]}"
    admin = create_async_engine(admin_url + "/postgres", isolation_level="AUTOCOMMIT")
    try:
        async with admin.connect() as conn:
            from sqlalchemy import text

            await conn.execute(text(f'CREATE DATABASE "{dbname}"'))
    except Exception as exc:
        await admin.dispose()
        _unavailable(f"test PostgreSQL unreachable ({exc})")

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


@pytest.fixture
async def client(pg_database):
    """An HTTP client against the real ASGI app, authenticated.

    Calling route functions directly leaves FastAPI's `Query(...)` defaults
    unresolved (the parameter arrives as a Query object), and skips the query
    parsing, validation and serialization that are most of what a route does.
    Going through the app tests the surface an operator actually hits.
    """
    import httpx

    from openrmm.api.app import create_app
    from openrmm.api.deps import current_user
    from openrmm.models.org import DEFAULT_ORG_ID
    from openrmm.models.user import Role, User

    app = create_app()
    # org_id matters now that the boundary is enforced. DEFAULT_ORG_ID is what
    # every model defaults to, so a fixture user without one would fail closed
    # against every device the tests create, which is correct behaviour and
    # useless for testing anything else.
    app.dependency_overrides[current_user] = lambda: User(
        id=uuid.uuid4(),
        email="admin@example.com",
        password_hash="x",
        role=Role.admin,
        org_id=DEFAULT_ORG_ID,
    )
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as c:
        yield c
    app.dependency_overrides.clear()


@pytest.fixture
async def client_as(pg_database):
    """A client factory that authenticates as a chosen role.

    The `client` fixture above overrides current_user with a hardcoded admin, so
    every HTTP test in this suite has always run with full privileges. That made
    a missing role dependency structurally undetectable: `require_role` had zero
    tests, and two routes serving fleet credentials and root-shell recordings to
    any authenticated user passed a green suite for months.

    Usage: `async with client_as(Role.viewer) as c: ...`
    """
    import contextlib

    import httpx

    from openrmm.api.app import create_app
    from openrmm.api.deps import current_user
    from openrmm.models.org import DEFAULT_ORG_ID
    from openrmm.models.user import User

    @contextlib.asynccontextmanager
    async def make(role):
        app = create_app()
        app.dependency_overrides[current_user] = lambda: User(
            id=uuid.uuid4(),
            email=f"{role.value}@example.com",
            password_hash="x",
            role=role,
            org_id=DEFAULT_ORG_ID,
        )
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as c:
            yield c
        app.dependency_overrides.clear()

    return make
