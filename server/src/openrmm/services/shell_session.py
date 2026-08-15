"""Browser WebSocket <-> NATS <-> agent PTY bridge.

The hard part is backpressure: core NATS will not buffer for a slow consumer,
so the agent must be told when to stop reading its PTY. The protocol (see
docs/nats-subjects.md) is an explicit byte-ack window:

  agent -> .out   raw frames (<=32 KiB), counts un-acked bytes
  server -> .ctl  {"ack": n} AFTER the browser websocket write completes
  agent pauses PTY reads above 512 KiB un-acked

Because the ack is emitted only after `await websocket.send_bytes(...)`
returns, TCP backpressure from a slow browser propagates all the way to the
remote PTY. A `yes` flood therefore throttles instead of drowning anyone.
"""

import asyncio
import contextlib
import json
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path

import nats
import structlog
from fastapi import WebSocket, WebSocketDisconnect

from openrmm.config import get_settings
from openrmm.natsio.subjects import cmd, shell_ctl, shell_in, shell_out, shell_resize

log = structlog.get_logger()

PING_INTERVAL_S = 30
OPEN_TIMEOUT_S = 5
QUEUE_MAXSIZE = 256


class AsciicastRecorder:
    """asciicast v2: a JSON header line, then [elapsed, "o", data] events."""

    def __init__(self, path: Path, cols: int, rows: int) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = path.open("w", encoding="utf-8")
        self._start = time.monotonic()
        header = {
            "version": 2,
            "width": cols,
            "height": rows,
            "timestamp": int(time.time()),
            "env": {"TERM": "xterm-256color"},
        }
        self._fh.write(json.dumps(header) + "\n")

    def write_output(self, data: bytes) -> None:
        event = [
            round(time.monotonic() - self._start, 6),
            "o",
            data.decode("utf-8", errors="replace"),
        ]
        self._fh.write(json.dumps(event) + "\n")

    def close(self) -> None:
        with contextlib.suppress(Exception):
            self._fh.close()


async def bridge_shell(
    websocket: WebSocket,
    nc: nats.NATS,
    device_id: uuid.UUID,
    *,
    requested_by: str,
    cols: int = 80,
    rows: int = 24,
    shell: str = "auto",
    record_dir: Path | None = None,
) -> tuple[uuid.UUID, str, int, int]:
    """Run one shell session. Returns (session_id, close_reason, bytes_in, bytes_out)."""
    settings = get_settings()
    session_id = uuid.uuid4()
    agent_id = str(device_id)
    sid = str(session_id)
    close_reason = "client"
    bytes_in = bytes_out = 0

    reply = await nc.request(
        cmd(agent_id, "shell.open"),
        json.dumps(
            {
                "session_id": sid,
                "shell": shell,
                "cols": cols,
                "rows": rows,
                "idle_timeout_s": 900,
                "requested_by": requested_by,
            }
        ).encode(),
        timeout=OPEN_TIMEOUT_S,
    )
    ack = json.loads(reply.data)
    if not ack.get("accepted"):
        raise RuntimeError(ack.get("error") or "agent refused the session")

    recorder = (
        AsciicastRecorder(record_dir / f"{sid}.cast", cols, rows)
        if record_dir is not None
        else None
    )
    queue: asyncio.Queue[bytes] = asyncio.Queue(maxsize=QUEUE_MAXSIZE)
    dropped = 0

    async def on_out(msg) -> None:
        nonlocal dropped
        try:
            queue.put_nowait(msg.data)
        except asyncio.QueueFull:
            # Pathological: browser is far behind even with the ack window.
            # Drop oldest so the newest output still flows.
            with contextlib.suppress(asyncio.QueueEmpty):
                queue.get_nowait()
            dropped += 1
            queue.put_nowait(msg.data)

    async def on_ctl(msg) -> None:
        nonlocal close_reason
        try:
            event = json.loads(msg.data)
        except json.JSONDecodeError:
            return
        if event.get("event") == "closed":
            close_reason = event.get("reason", "exit")
            await queue.put(b"")  # sentinel: stop the pump
        elif event.get("event") == "gap":
            await queue.put(b"\r\n\x1b[33m[output gap: agent was disconnected]\x1b[0m\r\n")

    sub_out = await nc.subscribe(shell_out(agent_id, sid), cb=on_out)
    sub_ctl = await nc.subscribe(shell_ctl(agent_id, sid), cb=on_ctl)

    async def pump_to_browser() -> None:
        nonlocal bytes_out
        while True:
            data = await queue.get()
            if data == b"":
                return
            # awaited write => browser TCP backpressure reaches us here
            await websocket.send_bytes(data)
            bytes_out += len(data)
            if recorder is not None:
                recorder.write_output(data)
            # ack AFTER the write: this is the flow-control signal
            await nc.publish(shell_ctl(agent_id, sid), json.dumps({"ack": len(data)}).encode())

    async def pump_to_agent() -> None:
        nonlocal bytes_in
        while True:
            message = await websocket.receive()
            if message["type"] == "websocket.disconnect":
                raise WebSocketDisconnect(message.get("code", 1000))
            if (payload := message.get("bytes")) is not None:
                await nc.publish(shell_in(agent_id, sid), payload)
                bytes_in += len(payload)
            elif (text := message.get("text")) is not None:
                # control frames from the browser are JSON text
                try:
                    event = json.loads(text)
                except json.JSONDecodeError:
                    continue
                if event.get("type") == "resize":
                    await nc.publish(
                        shell_resize(agent_id, sid),
                        json.dumps(
                            {"cols": int(event["cols"]), "rows": int(event["rows"])}
                        ).encode(),
                    )

    async def ping_agent() -> None:
        while True:
            await asyncio.sleep(PING_INTERVAL_S)
            await nc.publish(shell_ctl(agent_id, sid), json.dumps({"event": "ping"}).encode())

    tasks = [
        asyncio.create_task(pump_to_browser(), name="shell-out"),
        asyncio.create_task(pump_to_agent(), name="shell-in"),
        asyncio.create_task(ping_agent(), name="shell-ping"),
    ]
    try:
        done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_EXCEPTION)
        for task in done:
            exc = task.exception()
            if isinstance(exc, WebSocketDisconnect):
                close_reason = "client"
            elif exc is not None:
                close_reason = "error"
                log.warning("shell bridge task failed", error=str(exc))
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        with contextlib.suppress(Exception):
            await nc.request(
                cmd(agent_id, "shell.close"),
                json.dumps({"session_id": sid}).encode(),
                timeout=2,
            )
        await sub_out.unsubscribe()
        await sub_ctl.unsubscribe()
        if recorder is not None:
            recorder.close()
        if dropped:
            log.warning("shell output frames dropped", session_id=sid, dropped=dropped)
        log.info(
            "shell session closed",
            session_id=sid,
            reason=close_reason,
            bytes_in=bytes_in,
            bytes_out=bytes_out,
            settings_mode=settings.mode,
        )

    return session_id, close_reason, bytes_in, bytes_out


def utcnow() -> datetime:
    return datetime.now(UTC)
