"""Gotify push. Token is an app token and travels in the query string."""

import httpx

from openrmm.alerting.channels.base import (
    HTTP_TIMEOUT,
    USER_AGENT,
    ChannelError,
    Notification,
    check_response,
)

PRIORITIES = {"info": 3, "warning": 7, "critical": 9}


class GotifyChannel:
    def __init__(self, config: dict, *, transport: httpx.AsyncBaseTransport | None = None) -> None:
        url = str(config.get("url") or "").strip().rstrip("/")
        if not url.startswith(("http://", "https://")):
            raise ChannelError(f"gotify url is missing or not http(s): {url!r}", permanent=True)
        token = str(config.get("token") or "").strip()
        if not token:
            raise ChannelError("gotify channel has no token", permanent=True)
        self.url = url
        self.token = token
        self._transport = transport

    async def send(self, note: Notification) -> None:
        payload = {
            "title": note.title,
            "message": note.body or note.title,
            "priority": PRIORITIES.get(note.severity, PRIORITIES["info"]),
        }
        async with httpx.AsyncClient(timeout=HTTP_TIMEOUT, transport=self._transport) as client:
            try:
                response = await client.post(
                    f"{self.url}/message",
                    params={"token": self.token},
                    json=payload,
                    headers={"User-Agent": USER_AGENT},
                )
            except httpx.HTTPError as exc:
                raise ChannelError(f"gotify request failed: {exc}") from exc
        check_response(response, "gotify")
