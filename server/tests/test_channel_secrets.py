"""Channel credentials must not reach the browser, and must survive an edit.

`config` was returned raw, so every session that listed channels received the
webhook HMAC signing secret in plaintext. That secret lets anyone forge signed
deliveries; a gotify token posts as you.

Removing it creates the second problem these cover: an edit form cannot echo
back a value it was never given, so a naive PUT would clear the credential
while renaming the channel, and the only symptom would be deliveries failing a
signature check somewhere else entirely.
"""

import pytest

from openrmm.db.engine import get_sessionmaker
from openrmm.models.alert import ChannelKind, NotificationChannel
from openrmm.schemas.alert import SECRET_CONFIG_KEYS, ChannelOut

pytestmark = pytest.mark.usefixtures("pg_database")

WEBHOOK = {"url": "https://hooks.example.com/openrmm", "secret": "hmac-signing-secret"}


async def _channel(kind=ChannelKind.webhook, config=None) -> NotificationChannel:
    async with get_sessionmaker()() as db, db.begin():
        c = NotificationChannel(name=f"ch-{kind.value}", kind=kind, config=config or dict(WEBHOOK))
        db.add(c)
    return c


def test_the_secret_keys_are_the_ones_that_matter():
    """A key added to a channel kind without being listed here leaks."""
    assert frozenset({"secret", "token"}) == SECRET_CONFIG_KEYS


async def test_the_signing_secret_never_reaches_the_client():
    c = await _channel()
    out = ChannelOut.redacted(c)
    assert "secret" not in out.config
    assert "hmac-signing-secret" not in out.model_dump_json()


async def test_non_secret_config_is_still_returned():
    """Redaction must not blind the operator to the thing they need to check,
    which for a webhook is the URL."""
    c = await _channel()
    out = ChannelOut.redacted(c)
    assert out.config["url"] == "https://hooks.example.com/openrmm"


async def test_the_client_is_told_a_secret_exists():
    """Absent and absent-but-set look identical otherwise, so the UI cannot
    say whether the channel is configured."""
    c = await _channel()
    assert ChannelOut.redacted(c).secrets_set == ["secret"]

    plain = await _channel(kind=ChannelKind.ntfy, config={"url": "https://ntfy.sh", "topic": "t"})
    assert ChannelOut.redacted(plain).secrets_set == []


async def test_an_empty_secret_does_not_count_as_set():
    c = await _channel(config={"url": "https://x.example", "secret": ""})
    assert ChannelOut.redacted(c).secrets_set == []


async def test_editing_without_the_secret_preserves_it(client):
    """The whole point. Rename a channel from a form that never saw the
    secret, and the secret must still be there."""
    c = await _channel()

    r = await client.put(
        f"/api/v1/alerts/channels/{c.id}",
        json={
            "name": "renamed",
            "kind": "webhook",
            "config": {"url": "https://hooks.example.com/openrmm"},
            "enabled": True,
        },
    )
    r.raise_for_status()
    assert r.json()["secrets_set"] == ["secret"]

    async with get_sessionmaker()() as db:
        stored = await db.get(NotificationChannel, c.id)
    assert stored.name == "renamed"
    assert stored.config["secret"] == "hmac-signing-secret", (
        "renaming the channel cleared its signing secret; deliveries would "
        "start failing signature checks with nothing here to say why"
    )


async def test_sending_a_new_secret_replaces_it(client):
    """Preserving must not mean unchangeable: rotating a leaked secret has to
    be possible."""
    c = await _channel()
    r = await client.put(
        f"/api/v1/alerts/channels/{c.id}",
        json={
            "name": "ch-webhook",
            "kind": "webhook",
            "config": {"url": "https://hooks.example.com/openrmm", "secret": "rotated"},
            "enabled": True,
        },
    )
    r.raise_for_status()

    async with get_sessionmaker()() as db:
        stored = await db.get(NotificationChannel, c.id)
    assert stored.config["secret"] == "rotated"


async def test_a_round_trip_through_the_api_does_not_write_a_mask(client):
    """The failure a masked placeholder would cause: GET, edit a field, PUT
    the object back, and the mask is now the secret."""
    c = await _channel()

    got = (await client.get("/api/v1/alerts/channels")).json()
    mine = next(x for x in got if x["id"] == str(c.id))
    mine["name"] = "round-tripped"

    r = await client.put(f"/api/v1/alerts/channels/{c.id}", json=mine)
    r.raise_for_status()

    async with get_sessionmaker()() as db:
        stored = await db.get(NotificationChannel, c.id)
    assert stored.config["secret"] == "hmac-signing-secret"
