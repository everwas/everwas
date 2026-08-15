import httpx
import pytest

from openrmm import __version__
from openrmm.api.app import create_app
from openrmm.api.deps import db_session
from openrmm.natsio import subjects


class FakeDb:
    async def execute(self, *_args, **_kwargs):
        return None


@pytest.fixture
def app():
    app = create_app()

    async def fake_session():
        yield FakeDb()

    app.dependency_overrides[db_session] = fake_session
    return app


async def test_health(app):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        r = await client.get("/api/v1/health")
    assert r.status_code == 200
    assert r.json() == {"status": "ok", "version": __version__}


async def test_me_requires_auth(app):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        r = await client.get("/api/v1/auth/me")
    assert r.status_code == 401


def test_agent_permissions_are_scoped():
    perms = subjects.agent_permissions("abc")
    assert "agents.abc.>" in perms["publish"]
    assert all("abc" in s or s.startswith("_INBOX") for s in perms["publish"])
    assert all("abc" in s or s.startswith("_INBOX") for s in perms["subscribe"])


def test_subject_builders_match_contract():
    assert subjects.heartbeat("x") == "agents.x.heartbeat"
    assert subjects.jobs_queue("x") == "jobs.x"
    assert subjects.cmd("x", "shell.open") == "cmd.x.shell.open"
    assert subjects.shell_in("x", "s1") == "agents.x.shell.s1.in"
