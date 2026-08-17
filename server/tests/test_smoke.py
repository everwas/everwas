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

    This test could not fail until now. It iterated `app.routes` looking for
    APIRoute, and on this FastAPI version routers included with a prefix appear
    as `_IncludedRouter` wrappers whose children are reachable only through
    `original_router.routes`. The loop matched zero routes, so `unguarded` was
    always empty and the assertion always held, whatever the code did, while
    reading as proof that the whole API was authenticated. It walks properly
    now, and covers the shell WebSocket too, which authenticates by hand inside
    its handler and was previously exempt by construction.
    """
    from routewalk import api_routes

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

    # Authenticates inside the handler rather than through a dependency,
    # because a WebSocket must be refused BEFORE accept() and a dependency that
    # raises would have already completed the handshake. Listed explicitly so
    # the exemption is a decision rather than an oversight; the authorization
    # it performs is covered by tests/test_role_gates.py and
    # tests/test_org_isolation.py.
    HAND_AUTHENTICATED = {"/api/v1/devices/{device_id}/shell"}

    app = create_app()
    routes = api_routes(app, websockets=True)
    assert len(routes) > 20, (
        f"only {len(routes)} routes found; the walk is broken and this test proves nothing"
    )

    unguarded = []
    for route in routes:
        if route.path in PUBLIC or route.path in HAND_AUTHENTICATED:
            continue
        names = {d.name for d in route.dependant.dependencies}
        flat = str(route.dependant.dependencies) + str(names)
        if "current_user" not in flat and "check" not in flat:
            unguarded.append(f"{sorted(getattr(route, 'methods', []) or ['WS'])} {route.path}")

    assert not unguarded, f"routes without authentication: {unguarded}"


def test_agents_can_publish_to_the_reply_subject_the_server_uses():
    """The regression that broke every server-to-agent command.

    Pinning the agent's publish grant to its own namespace also cut off the
    default `_INBOX.…` that `nc.request()` replies on, so agents received
    commands, executed them, and were REFUSED when they answered. The server
    saw a plain timeout. Credential rotation applied on the agent and reported
    failure to the operator, which is how a machine gets locked out.

    Every server-to-agent request therefore has to reply inside the agent's own
    inbox namespace. This asserts the two halves still agree.
    """
    from openrmm.natsio.agent_request import reply_subject
    from openrmm.natsio.subjects import agent_permissions

    agent_id = "01a00b45-0e50-78c8-b572-8b8fbc272ad1"
    subject = reply_subject(agent_id)
    allowed = agent_permissions(agent_id)["publish"]

    assert _subject_allowed(subject, allowed), (
        f"the agent may not publish to {subject}, so it cannot answer any command; "
        f"grants are {allowed}"
    )


def test_the_server_reply_subject_is_not_shared_between_agents():
    """It has to be per-agent, or the fix reintroduces the hole it came from."""
    from openrmm.natsio.agent_request import reply_subject
    from openrmm.natsio.subjects import agent_permissions

    mine = "01a00b45-0e50-78c8-b572-8b8fbc272ad1"
    theirs = "01a00710-f7a8-7a84-8986-2081f6ac56c6"

    assert not _subject_allowed(reply_subject(theirs), agent_permissions(mine)["publish"]), (
        "one agent can publish into another agent's server reply inbox and forge its answers"
    )


def _subject_allowed(subject: str, grants: list[str]) -> bool:
    """NATS subject matching: `*` is one token, `>` is the rest."""
    tokens = subject.split(".")
    for grant in grants:
        gt = grant.split(".")
        for i, g in enumerate(gt):
            if g == ">":
                return i <= len(tokens)
            if i >= len(tokens) or (g != "*" and g != tokens[i]):
                break
        else:
            if len(gt) == len(tokens):
                return True
    return False
