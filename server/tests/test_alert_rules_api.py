"""The alert rule API refuses to create a rule that can never tell anyone.

A rule with no notification channels fires forever into the void: alerts
accumulate, nobody is paged, and every surface an operator looks at is
consistent with a healthy fleet. The API accepted `channel_ids: []` without a
word.
"""

import uuid

import httpx
import pytest

from openrmm.api.app import create_app
from openrmm.api.deps import current_user
from openrmm.db.deps import db_session
from openrmm.db.engine import get_sessionmaker
from openrmm.models.alert import ChannelKind, NotificationChannel
from openrmm.models.user import Role, User

pytestmark = pytest.mark.usefixtures("pg_database")

RULE = {
    "name": "cpu-high",
    "metric": "cpu",
    "operator": "gt",
    "threshold": 90,
    "duration_s": 300,
    "severity": "critical",
    "target": {"all": True},
    "cooldown_s": 900,
    "enabled": True,
    "channel_ids": [],
}


@pytest.fixture
def client():
    app = create_app()
    operator = User(id=uuid.uuid4(), email="ops@example.com", password_hash="x", role=Role.operator)

    async def session():
        async with get_sessionmaker()() as db:
            yield db

    app.dependency_overrides[db_session] = session
    app.dependency_overrides[current_user] = lambda: operator
    return httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test")


async def make_channel() -> uuid.UUID:
    channel = NotificationChannel(
        id=uuid.uuid4(),
        name=f"ops-{uuid.uuid4().hex[:6]}",
        kind=ChannelKind.webhook,
        config={"url": "https://hooks.example.com/rmm"},
        enabled=True,
    )
    async with get_sessionmaker()() as db, db.begin():
        db.add(channel)
    return channel.id


async def test_an_enabled_rule_needs_somewhere_to_send(client):
    async with client as http:
        r = await http.post("/api/v1/alerts/rules", json=RULE)
    assert r.status_code == 422
    assert "void" in r.json()["detail"]


async def test_a_disabled_rule_may_be_saved_without_channels(client):
    """Drafting a rule before its channel exists is legitimate."""
    async with client as http:
        r = await http.post(
            "/api/v1/alerts/rules", json={**RULE, "enabled": False, "name": "draft"}
        )
    assert r.status_code == 201


async def test_a_rule_pointed_at_a_channel_that_does_not_exist_is_refused(client):
    async with client as http:
        r = await http.post(
            "/api/v1/alerts/rules", json={**RULE, "channel_ids": [str(uuid.uuid4())]}
        )
    assert r.status_code == 422
    assert "unknown notification channel" in r.json()["detail"]


async def test_a_rule_with_a_channel_is_created(client):
    channel_id = await make_channel()
    async with client as http:
        r = await http.post("/api/v1/alerts/rules", json={**RULE, "channel_ids": [str(channel_id)]})
    assert r.status_code == 201
    assert r.json()["channel_ids"] == [str(channel_id)]


async def test_an_edit_cannot_silence_a_rule_either(client):
    channel_id = await make_channel()
    async with client as http:
        created = await http.post(
            "/api/v1/alerts/rules", json={**RULE, "channel_ids": [str(channel_id)]}
        )
        rule_id = created.json()["id"]
        r = await http.put(f"/api/v1/alerts/rules/{rule_id}", json=RULE)
    assert r.status_code == 422
