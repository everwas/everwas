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
    """Every grant must name this agent, except the shared JS/inbox plumbing."""
    perms = subjects.agent_permissions("abc")
    shared = {"_INBOX.>", "$JS.API.INFO", "$JS.ACK.>"}
    assert "agents.abc.>" in perms["publish"]
    for subject in perms["publish"] + perms["subscribe"]:
        assert subject in shared or "abc" in subject, subject


def test_agent_jetstream_grants_are_own_durable_only():
    perms = subjects.agent_permissions("abc")
    js = [s for s in perms["publish"] if s.startswith("$JS.API.CONSUMER")]
    assert js, "agent needs JS consumer access for durable job delivery"
    # never a wildcard over other agents' consumers
    assert all("agent-abc" in s for s in js)
    assert not any(s.endswith("JOBS.>") or s.endswith("JOBS.*") for s in js)


def test_subject_builders_match_contract():
    assert subjects.heartbeat("x") == "agents.x.heartbeat"
    assert subjects.jobs_queue("x") == "jobs.x"
    assert subjects.cmd("x", "shell.open") == "cmd.x.shell.open"
    assert subjects.shell_in("x", "s1") == "agents.x.shell.s1.in"


def test_device_tags_support_array_operators():
    """Tag targeting compiles to a real SQL operator.

    Regression: models used the generic sqlalchemy.ARRAY, whose comparator has
    no overlap(), so every tag-targeted script run and patch policy raised
    AttributeError at query-build time.
    """
    from openrmm.models.device import Device

    assert "&&" in str(Device.tags.overlap(["prod"]))
    assert "@>" in str(Device.tags.contains(["prod"]))
