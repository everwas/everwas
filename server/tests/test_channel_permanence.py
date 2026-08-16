"""Which delivery failures are worth retrying, and which are not.

Getting this wrong is expensive in both directions: a permanent classification
destroys a page that would have gone through, and a transient one burns five
retries over 35 minutes on an address that will never accept mail while the
operator reads "retry scheduled" and assumes it is in hand.
"""

import aiosmtplib
import httpx
import pytest

from openrmm.alerting.channels.base import HTTP_TIMEOUT, ChannelError
from openrmm.alerting.channels.email import EmailChannel
from openrmm.services.outbox import DELIVERY_TIMEOUT_S


@pytest.fixture
def smtp_settings(monkeypatch):
    from openrmm.config import get_settings

    monkeypatch.setenv("OPENRMM_SMTP_HOST", "mailpit")
    monkeypatch.setenv("OPENRMM_SMTP_PORT", "1025")
    monkeypatch.setenv("OPENRMM_SMTP_FROM", "openrmm@example.com")
    get_settings.cache_clear()
    yield
    get_settings.cache_clear()


async def test_refused_recipients_are_permanent(monkeypatch, smtp_settings):
    """SMTPRecipientsRefused is an SMTPException but NOT an SMTPResponseException,
    so it fell through to the transient handler."""
    refusal = aiosmtplib.errors.SMTPRecipientRefused(550, "no such user", "gone@example.com")

    async def fake_send(message, **kwargs):
        raise aiosmtplib.SMTPRecipientsRefused([refusal])

    monkeypatch.setattr(aiosmtplib, "send", fake_send)
    with pytest.raises(ChannelError) as excinfo:
        await EmailChannel({"to": "gone@example.com"}).send(_note())

    assert excinfo.value.permanent is True, "35 minutes of retries for a dead mailbox"
    assert "gone@example.com" in str(excinfo.value), "the operator needs to know WHICH address"


async def test_a_4xx_smtp_response_is_still_transient(monkeypatch, smtp_settings):
    async def fake_send(message, **kwargs):
        raise aiosmtplib.SMTPResponseException(451, "greylisted, try later")

    monkeypatch.setattr(aiosmtplib, "send", fake_send)
    with pytest.raises(ChannelError) as excinfo:
        await EmailChannel({"to": "ops@example.com"}).send(_note())
    assert excinfo.value.permanent is False


def test_http_timeouts_are_per_phase_and_bounded_overall():
    """A per-operation timeout does not bound a server that dribbles bytes.

    httpx.Timeout(10) resets on every byte read, so the only real ceiling is
    the wall clock the outbox drainer wraps around the whole delivery.
    """
    assert isinstance(HTTP_TIMEOUT, httpx.Timeout)
    assert HTTP_TIMEOUT.connect is not None
    assert HTTP_TIMEOUT.read is not None
    assert HTTP_TIMEOUT.write is not None
    assert HTTP_TIMEOUT.pool is not None
    # the outer bound has to be larger than any single phase, or it is the
    # only timeout that ever fires and the phase limits are decoration
    assert (
        max(HTTP_TIMEOUT.connect, HTTP_TIMEOUT.read, HTTP_TIMEOUT.write, HTTP_TIMEOUT.pool)
        < DELIVERY_TIMEOUT_S
    )


def _note():
    from openrmm.alerting.channels.base import Notification

    return Notification(
        kind="alert.firing",
        title="CPU high on web-01",
        body="cpu_pct 99 over threshold 90",
        severity="critical",
        device_hostname="web-01",
    )
