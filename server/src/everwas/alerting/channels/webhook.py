"""Generic JSON webhook with optional HMAC body signing."""

import hashlib
import hmac
import json
from datetime import UTC, datetime

import httpx

from everwas.alerting.channels.base import (
    HTTP_TIMEOUT,
    USER_AGENT,
    ChannelError,
    Notification,
    check_response,
)

SIGNATURE_HEADER = "X-Everwas-Signature"


class WebhookChannel:
    def __init__(self, config: dict, *, transport: httpx.AsyncBaseTransport | None = None) -> None:
        url = str(config.get("url") or "").strip()
        if not url.startswith(("http://", "https://")):
            raise ChannelError(f"webhook url is missing or not http(s): {url!r}", permanent=True)
        self.url = url
        self.secret = str(config.get("secret") or "") or None
        self._transport = transport

    async def send(self, note: Notification) -> None:
        body = json.dumps(self.request_body(note), separators=(",", ":")).encode()
        headers = {
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        }
        if self.secret:
            headers[SIGNATURE_HEADER] = sign(self.secret, body)

        async with httpx.AsyncClient(timeout=HTTP_TIMEOUT, transport=self._transport) as client:
            try:
                response = await client.post(self.url, content=body, headers=headers)
            except httpx.HTTPError as exc:
                raise ChannelError(f"webhook request failed: {exc}") from exc
        check_response(response, "webhook")

    @staticmethod
    def request_body(note: Notification) -> dict:
        return {
            "kind": note.kind,
            "title": note.title,
            "body": note.body,
            "severity": note.severity,
            "device": note.device_hostname,
            "alert_id": note.alert_id,
            "context": note.context,
            "sent_at": datetime.now(UTC).isoformat(),
        }


def sign(secret: str, body: bytes) -> str:
    """Signature covers the exact bytes on the wire, not a re-serialization."""
    digest = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return f"sha256={digest}"
