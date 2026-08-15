"""Channel adapters, exercised without a network or a database."""

import hashlib
import hmac
import json

import httpx
import pytest

from openrmm.alerting.channels.base import (
    ChannelError,
    Notification,
    build_channel,
)
from openrmm.alerting.channels.email import EmailChannel, build_message, subject_for
from openrmm.alerting.channels.gotify import GotifyChannel
from openrmm.alerting.channels.ntfy import NtfyChannel
from openrmm.alerting.channels.webhook import SIGNATURE_HEADER, WebhookChannel
from openrmm.services.outbox import BACKOFF_S, backoff_for


def note(severity: str = "critical") -> Notification:
    return Notification(
        kind="alert.firing",
        title="CPU high on web-01",
        body="cpu_pct 96.4 over threshold 90 for 5m",
        severity=severity,
        device_hostname="web-01",
        alert_id="0198c0de-0000-7000-8000-000000000001",
        context={"rule": "cpu-high", "metric": "cpu", "threshold": 90, "last_value": 96.4},
    )


def recorder(status: int = 200, text: str = "ok"):
    """MockTransport that stashes the request it was handed."""
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        request.read()
        seen.append(request)
        return httpx.Response(status, text=text)

    return httpx.MockTransport(handler), seen


# --- webhook ---------------------------------------------------------------


async def test_webhook_signs_the_exact_body_bytes():
    transport, seen = recorder()
    await WebhookChannel(
        {"url": "https://hooks.example.com/rmm", "secret": "s3cret"}, transport=transport
    ).send(note())

    request = seen[0]
    expected = hmac.new(b"s3cret", request.content, hashlib.sha256).hexdigest()
    assert request.headers[SIGNATURE_HEADER] == f"sha256={expected}"

    body = json.loads(request.content)
    assert body["device"] == "web-01"
    assert body["severity"] == "critical"
    assert body["context"]["rule"] == "cpu-high"
    assert body["sent_at"].endswith("+00:00")
    assert request.headers["user-agent"].startswith("OpenRMM/")


async def test_webhook_unsigned_without_secret():
    transport, seen = recorder()
    await WebhookChannel({"url": "https://hooks.example.com/rmm"}, transport=transport).send(note())
    assert SIGNATURE_HEADER.lower() not in seen[0].headers


@pytest.mark.parametrize(
    ("status", "permanent"),
    [(400, True), (403, True), (404, True), (408, False), (429, False), (500, False), (503, False)],
)
async def test_webhook_status_permanence(status: int, permanent: bool):
    transport, _ = recorder(status, "nope")
    channel = WebhookChannel({"url": "https://hooks.example.com/rmm"}, transport=transport)
    with pytest.raises(ChannelError) as excinfo:
        await channel.send(note())
    assert excinfo.value.permanent is permanent


async def test_webhook_rejects_bad_url():
    with pytest.raises(ChannelError) as excinfo:
        WebhookChannel({"url": "not-a-url"})
    assert excinfo.value.permanent is True


# --- ntfy ------------------------------------------------------------------


@pytest.mark.parametrize(
    ("severity", "priority", "tags"),
    [
        ("info", "3", "information_source"),
        ("warning", "4", "warning"),
        ("critical", "5", "rotating_light"),
    ],
)
async def test_ntfy_priority_and_tags(severity: str, priority: str, tags: str):
    transport, seen = recorder()
    await NtfyChannel(
        {"url": "https://ntfy.example.com/", "topic": "rmm"}, transport=transport
    ).send(note(severity))

    request = seen[0]
    assert str(request.url) == "https://ntfy.example.com/rmm"
    assert request.headers["Priority"] == priority
    assert request.headers["Tags"] == tags
    assert request.headers["Title"] == "CPU high on web-01"
    assert b"cpu_pct 96.4" in request.content
    assert "authorization" not in request.headers


async def test_ntfy_bearer_token_when_configured():
    transport, seen = recorder()
    await NtfyChannel({"topic": "rmm", "token": "tk_123"}, transport=transport).send(note())
    assert seen[0].headers["Authorization"] == "Bearer tk_123"
    assert str(seen[0].url) == "https://ntfy.sh/rmm"


async def test_ntfy_requires_topic():
    with pytest.raises(ChannelError):
        NtfyChannel({"url": "https://ntfy.example.com"})


# --- gotify ----------------------------------------------------------------


@pytest.mark.parametrize(("severity", "priority"), [("info", 3), ("warning", 7), ("critical", 9)])
async def test_gotify_priority_and_token(severity: str, priority: int):
    transport, seen = recorder()
    await GotifyChannel(
        {"url": "https://gotify.example.com/", "token": "Atok"}, transport=transport
    ).send(note(severity))

    request = seen[0]
    assert request.url.path == "/message"
    assert request.url.params["token"] == "Atok"
    body = json.loads(request.content)
    assert body == {
        "title": "CPU high on web-01",
        "message": "cpu_pct 96.4 over threshold 90 for 5m",
        "priority": priority,
    }


async def test_gotify_requires_token():
    with pytest.raises(ChannelError) as excinfo:
        GotifyChannel({"url": "https://gotify.example.com"})
    assert excinfo.value.permanent is True


# --- email -----------------------------------------------------------------


@pytest.fixture
def smtp_settings(monkeypatch):
    from openrmm.config import get_settings

    monkeypatch.setenv("OPENRMM_SMTP_HOST", "mailpit")
    monkeypatch.setenv("OPENRMM_SMTP_PORT", "1025")
    monkeypatch.setenv("OPENRMM_SMTP_USER", "")
    monkeypatch.setenv("OPENRMM_SMTP_PASSWORD", "")
    monkeypatch.setenv("OPENRMM_SMTP_FROM", "openrmm@example.com")
    get_settings.cache_clear()
    yield
    get_settings.cache_clear()


async def test_email_sends_multipart_alternative(monkeypatch, smtp_settings):
    import aiosmtplib

    sent = {}

    async def fake_send(message, **kwargs):
        sent["message"] = message
        sent["kwargs"] = kwargs
        return {}, "ok"

    monkeypatch.setattr(aiosmtplib, "send", fake_send)
    await EmailChannel({"to": ["ops@example.com", "oncall@example.com"]}).send(note())

    message = sent["message"]
    assert message["Subject"] == "[OpenRMM][critical] CPU high on web-01"
    assert message["To"] == "ops@example.com, oncall@example.com"
    assert message["From"] == "openrmm@example.com"

    types = {part.get_content_type() for part in message.walk() if not part.is_multipart()}
    assert types == {"text/plain", "text/html"}

    html = message.get_body(("html",)).get_content()
    assert "#e34948" in html  # critical accent on the left border
    assert "CPU high on web-01" in html
    text = message.get_body(("plain",)).get_content()
    assert "cpu_pct 96.4" in text
    assert "rule: cpu-high" in text

    # no smtp_user configured means no auth attempt
    assert sent["kwargs"]["username"] is None
    assert sent["kwargs"]["hostname"] == "mailpit"


async def test_email_smtp_5xx_is_permanent(monkeypatch, smtp_settings):
    import aiosmtplib

    async def fake_send(message, **kwargs):
        raise aiosmtplib.SMTPResponseException(550, "mailbox unavailable")

    monkeypatch.setattr(aiosmtplib, "send", fake_send)
    with pytest.raises(ChannelError) as excinfo:
        await EmailChannel({"to": "ops@example.com"}).send(note())
    assert excinfo.value.permanent is True


async def test_email_connection_refused_is_transient(monkeypatch, smtp_settings):
    import aiosmtplib

    async def fake_send(message, **kwargs):
        raise ConnectionRefusedError("no smtp here")

    monkeypatch.setattr(aiosmtplib, "send", fake_send)
    with pytest.raises(ChannelError) as excinfo:
        await EmailChannel({"to": "ops@example.com"}).send(note())
    assert excinfo.value.permanent is False


def test_email_requires_recipients():
    with pytest.raises(ChannelError) as excinfo:
        EmailChannel({"to": []})
    assert excinfo.value.permanent is True


def test_email_subject_carries_severity_and_title():
    assert subject_for(note("warning")) == "[OpenRMM][warning] CPU high on web-01"
    message = build_message(note(), ["ops@example.com"], "openrmm@example.com")
    assert message["X-OpenRMM-Severity"] == "critical"


# --- factory and payload ---------------------------------------------------


def test_build_channel_rejects_unknown_kind():
    with pytest.raises(ChannelError) as excinfo:
        build_channel("carrier-pigeon", {})
    assert excinfo.value.permanent is True


def test_build_channel_returns_the_right_adapter():
    assert isinstance(build_channel("ntfy", {"topic": "rmm"}), NtfyChannel)
    assert isinstance(build_channel("webhook", {"url": "https://x.example.com"}), WebhookChannel)


def test_notification_round_trips_through_payload():
    payload = note().to_payload()
    assert Notification.from_payload(payload) == note()
    assert json.loads(json.dumps(payload)) == payload


def test_notification_from_payload_rejects_junk():
    for payload in ({}, {"kind": "test"}, {"title": "no kind"}, None):
        with pytest.raises(ChannelError) as excinfo:
            Notification.from_payload(payload)
        assert excinfo.value.permanent is True


def test_notification_defaults_unknown_severity_to_info():
    parsed = Notification.from_payload({"kind": "test", "title": "hi", "severity": "spicy"})
    assert parsed.severity == "info"
    assert parsed.context == {}


# --- retry schedule --------------------------------------------------------


def test_backoff_schedule():
    assert [backoff_for(n) for n in (1, 2, 3, 4)] == [60, 300, 1800, 3600]
    assert BACKOFF_S == (60, 300, 1800, 3600)
    assert backoff_for(5) == 3600
    assert backoff_for(99) == 3600
    assert backoff_for(0) == 60
