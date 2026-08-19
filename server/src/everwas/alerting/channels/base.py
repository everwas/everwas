"""Channel contract shared by every delivery adapter.

The permanent/transient split on ChannelError is what the outbox drainer uses to
decide between "give up now" and "back off and try again", so adapters have to
be deliberate about which one they raise.
"""

from dataclasses import dataclass, field
from typing import Any, Protocol, Self

import httpx

from everwas import __version__

HTTP_TIMEOUT_S = 10.0
# httpx timeouts are PER OPERATION, not a total budget: a server that dribbles
# one byte every 9 seconds resets the read timer forever and holds the request
# (and, upstream, the outbox transaction) open indefinitely. These bound each
# phase; services.outbox wraps the whole delivery in a wall-clock ceiling,
# which is the only thing that actually bounds a slow-loris endpoint.
HTTP_TIMEOUT = httpx.Timeout(
    connect=5.0,
    read=HTTP_TIMEOUT_S,
    write=HTTP_TIMEOUT_S,
    pool=5.0,
)
USER_AGENT = f"Everwas/{__version__}"
# 408 and 429 are 4xx but say "later", not "never"
RETRYABLE_4XX = {408, 429}
SEVERITIES = ("info", "warning", "critical")
SEVERITY_COLORS = {"info": "#2a78d6", "warning": "#eda100", "critical": "#e34948"}


class ChannelError(Exception):
    def __init__(self, message: str, *, permanent: bool = False) -> None:
        super().__init__(message)
        self.permanent = permanent


@dataclass(slots=True)
class Notification:
    kind: str  # alert.firing | alert.resolved | test
    title: str
    body: str
    severity: str = "info"
    device_hostname: str | None = None
    alert_id: str | None = None
    context: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_payload(cls, payload: dict) -> Self:
        """Rebuild from the jsonb column written by the alert engine."""
        if not isinstance(payload, dict) or not payload.get("kind") or not payload.get("title"):
            raise ChannelError(f"malformed notification payload: {payload!r}", permanent=True)
        severity = str(payload.get("severity") or "info")
        if severity not in SEVERITIES:
            severity = "info"
        device = payload.get("device_hostname", payload.get("device"))
        return cls(
            kind=str(payload["kind"]),
            title=str(payload["title"]),
            body=str(payload.get("body") or ""),
            severity=severity,
            device_hostname=str(device) if device else None,
            alert_id=str(payload["alert_id"]) if payload.get("alert_id") else None,
            context=payload.get("context") or {},
        )

    def to_payload(self) -> dict:
        return {
            "kind": self.kind,
            "title": self.title,
            "body": self.body,
            "severity": self.severity,
            "device_hostname": self.device_hostname,
            "alert_id": self.alert_id,
            "context": self.context,
        }


class Channel(Protocol):
    async def send(self, note: Notification) -> None: ...


def build_channel(kind: str, config: dict) -> Channel:
    from everwas.alerting.channels.email import EmailChannel
    from everwas.alerting.channels.gotify import GotifyChannel
    from everwas.alerting.channels.ntfy import NtfyChannel
    from everwas.alerting.channels.webhook import WebhookChannel

    builders: dict[str, type[Channel]] = {
        "email": EmailChannel,
        "webhook": WebhookChannel,
        "ntfy": NtfyChannel,
        "gotify": GotifyChannel,
    }
    builder = builders.get(str(kind))
    if builder is None:
        raise ChannelError(f"unknown channel kind {kind!r}", permanent=True)
    return builder(config or {})  # type: ignore[call-arg]


def header_safe(value: str, limit: int = 240) -> str:
    """Headers carry titles; strip anything that would break the wire format."""
    flat = " ".join(value.split())
    return flat.encode("ascii", "replace").decode()[:limit]


def check_response(response: httpx.Response, label: str) -> None:
    code = response.status_code
    if code < 400:
        return
    detail = response.text[:200].strip()
    permanent = code < 500 and code not in RETRYABLE_4XX
    raise ChannelError(f"{label} returned {code}: {detail}", permanent=permanent)
