"""ntfy.sh push. Body is the message, everything else rides in headers."""

import httpx

from everwas.alerting.channels.base import (
    HTTP_TIMEOUT,
    USER_AGENT,
    ChannelError,
    Notification,
    check_response,
    header_safe,
)

DEFAULT_URL = "https://ntfy.sh"
PRIORITIES = {"info": "3", "warning": "4", "critical": "5"}
TAGS = {"info": "information_source", "warning": "warning", "critical": "rotating_light"}


class NtfyChannel:
    def __init__(self, config: dict, *, transport: httpx.AsyncBaseTransport | None = None) -> None:
        self.base_url = str(config.get("url") or DEFAULT_URL).strip().rstrip("/")
        topic = str(config.get("topic") or "").strip().strip("/")
        if not topic:
            raise ChannelError("ntfy channel has no topic", permanent=True)
        self.topic = topic
        self.token = str(config.get("token") or "") or None
        self._transport = transport

    async def send(self, note: Notification) -> None:
        headers = {
            "Title": header_safe(note.title),
            "Priority": PRIORITIES.get(note.severity, PRIORITIES["info"]),
            "Tags": TAGS.get(note.severity, TAGS["info"]),
            "User-Agent": USER_AGENT,
        }
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"

        async with httpx.AsyncClient(timeout=HTTP_TIMEOUT, transport=self._transport) as client:
            try:
                response = await client.post(
                    f"{self.base_url}/{self.topic}",
                    content=(note.body or note.title).encode(),
                    headers=headers,
                )
            except httpx.HTTPError as exc:
                raise ChannelError(f"ntfy request failed: {exc}") from exc
        check_response(response, "ntfy")
