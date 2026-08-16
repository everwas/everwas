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


def test_agent_permissions_have_no_shared_subjects():
    """EVERY grant must name this agent. No exceptions.

    The previous version of this test listed the shared grants by name and
    excused them:

        shared = {"_INBOX.>", "$JS.API.INFO", "$JS.ACK.>"}

    Two of those three were the isolation failure. `_INBOX.>` let any enrolled
    agent receive every other agent's request replies and job deliveries
    (verified against a live server), and `$JS.ACK.>` let it forge acks on the
    server's own ingest consumers. The word "shared" was doing the work of a
    security argument, so the test passed while isolation did not hold.
    """
    perms = subjects.agent_permissions("abc")
    assert "agents.abc.>" in perms["publish"]
    for subject in perms["publish"] + perms["subscribe"]:
        assert "abc" in subject, f"grant is not agent-scoped: {subject}"


def test_agent_grants_never_widen_to_other_agents():
    """No grant may reach another agent's namespace, inbox, or consumer."""
    mine = subjects.agent_permissions("aaa")
    theirs = subjects.agent_permissions("bbb")

    for subject in mine["publish"] + mine["subscribe"]:
        assert "bbb" not in subject
        # A wildcard token where the agent id belongs would match everyone.
        assert not subject.startswith("_INBOX."), "must use a per-agent inbox prefix"
        assert not subject.startswith("$JS.ACK.>"), "acks must be scoped to our durable"

    assert set(mine["publish"]).isdisjoint(theirs["publish"])
    assert set(mine["subscribe"]).isdisjoint(theirs["subscribe"])


def test_consumer_create_grant_pins_the_filter_subject():
    """CONSUMER.CREATE carries the filter as a trailing token.

    Granting `...CREATE.JOBS.{durable}.>` would let an agent create its own
    consumer filtering `jobs.*` and drain the entire fleet's work, leaving the
    real devices to never receive their jobs.
    """
    perms = subjects.agent_permissions("abc")
    creates = [s for s in perms["publish"] if ".CONSUMER.CREATE." in s]
    assert creates, "agent needs consumer create for durable job delivery"
    for subject in creates:
        assert "agent-abc" in subject
        assert not subject.endswith(">"), f"filter subject is not pinned: {subject}"
        assert not subject.endswith("*"), f"filter subject is not pinned: {subject}"
    # exactly one literal filter, and it is this agent's own job subject
    assert f"$JS.API.CONSUMER.CREATE.JOBS.{subjects.agent_durable('abc')}.jobs.abc" in creates


def test_ack_grant_is_scoped_to_own_durable():
    perms = subjects.agent_permissions("abc")
    acks = [s for s in perms["publish"] if s.startswith("$JS.ACK")]
    assert acks == [f"$JS.ACK.JOBS.{subjects.agent_durable('abc')}.>"]


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


def test_every_route_requires_auth_except_the_documented_few():
    """No route may ship without authentication by accident.

    Regression: GET /devices/sessions/recent had no CurrentUser dependency, so
    an anonymous caller could enumerate device UUIDs and read admin shell
    history. Every other read route had one; this test makes the exception
    list explicit instead of relying on nobody forgetting.
    """
    from fastapi.routing import APIRoute

    from openrmm.api.app import create_app

    # Public by design: liveness, login, logout, and agent enrollment (which
    # authenticates with a one-time token in its body, not a session).
    PUBLIC = {
        "/api/v1/health",
        "/api/v1/health/ingest",
        "/api/v1/auth/login",
        "/api/v1/auth/logout",
        "/api/v1/agents/enroll",
    }

    app = create_app()
    unguarded = []
    for route in app.routes:
        if not isinstance(route, APIRoute) or route.path in PUBLIC:
            continue
        names = {d.name for d in route.dependant.dependencies}
        flat = str(route.dependant.dependencies) + str(names)
        if "current_user" not in flat and "check" not in flat:
            unguarded.append(f"{sorted(route.methods)} {route.path}")

    assert not unguarded, f"routes without authentication: {unguarded}"
