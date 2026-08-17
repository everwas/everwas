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
from openrmm.natsio.agent_request import request_agent
from openrmm.natsio.subjects import shell_ctl, shell_in, shell_out, shell_resize

log = structlog.get_logger()

PING_INTERVAL_S = 30
OPEN_TIMEOUT_S = 5
QUEUE_MAXSIZE = 256


class AsciicastRecorder:
    """asciicast v2: a JSON header line, then [elapsed, "o", data] events.

    Every write is buffered in memory and flushed off the event loop. The
    recorder used to call `self._fh.write(...)` directly from the output pump,
    which is a synchronous disk write on the loop thread for EVERY shell frame:
    a busy session, or one slow fsync, stalls every other websocket, HTTP
    request and NATS callback this worker is serving. Recording someone's
    terminal is not worth pausing the API.
    """

    #: Flush once the pending buffer passes this. Small enough that a crash
    #: loses very little, large enough that a `yes` flood is not one thread
    #: hop per frame.
    FLUSH_BYTES = 64 * 1024

    def __init__(self, path: Path, cols: int, rows: int) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = path.open("w", encoding="utf-8")
        self._start = time.monotonic()
        self._pending: list[str] = []
        self._pending_bytes = 0
        header = {
            "version": 2,
            "width": cols,
            "height": rows,
            "timestamp": int(time.time()),
            "env": {"TERM": "xterm-256color"},
        }
        self._append(json.dumps(header) + "\n")

    def _append(self, line: str) -> None:
        self._pending.append(line)
        self._pending_bytes += len(line)

    def write_output(self, data: bytes) -> None:
        """Buffer one output event. Cheap and never touches the disk."""
        event = [
            round(time.monotonic() - self._start, 6),
            "o",
            data.decode("utf-8", errors="replace"),
        ]
        self._append(json.dumps(event) + "\n")

    def write_resize(self, cols: int, rows: int) -> None:
        """Buffer a resize event so playback follows the real terminal size.

        asciicast v2 has an "r" event for this, and without it a recording
        replays at whatever size the session opened with. Anything the user
        resized to see (a wide table, a full-screen editor) then renders
        wrapped and unreadable in playback, which is precisely the session
        somebody goes back to watch.
        """
        event = [round(time.monotonic() - self._start, 6), "r", f"{cols}x{rows}"]
        self._append(json.dumps(event) + "\n")

    async def maybe_flush(self) -> None:
        if self._pending_bytes >= self.FLUSH_BYTES:
            await self.flush()

    async def flush(self) -> None:
        if not self._pending:
            return
        chunk, self._pending, self._pending_bytes = "".join(self._pending), [], 0
        # A recording is never worth failing a live session for.
        with contextlib.suppress(Exception):
            await asyncio.to_thread(self._fh.write, chunk)

    async def aclose(self) -> None:
        await self.flush()
        with contextlib.suppress(Exception):
            await asyncio.to_thread(self._fh.close)


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

    reply_data = await request_agent(
        nc,
        agent_id,
        "shell.open",
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
    ack = json.loads(reply_data)
    if not ack.get("accepted"):
        raise RuntimeError(ack.get("error") or "agent refused the session")

    recorder = (
        AsciicastRecorder(record_dir / f"{sid}.cast", cols, rows)
        if record_dir is not None
        else None
    )
    queue: asyncio.Queue[bytes] = asyncio.Queue(maxsize=QUEUE_MAXSIZE)
    # Set when the agent says the PTY is gone. An Event, not a queue sentinel:
    # a full queue must not be able to delay or lose the close.
    agent_closed = asyncio.Event()
    dropped = 0

    async def ack(n: int) -> None:
        """Credit `n` bytes back to the agent's flow-control window."""
        await nc.publish(shell_ctl(agent_id, sid), json.dumps({"ack": n}).encode())

    async def on_out(msg) -> None:
        nonlocal dropped
        try:
            queue.put_nowait(msg.data)
        except asyncio.QueueFull:
            # Pathological: browser is far behind even with the ack window.
            # Drop oldest so the newest output still flows.
            try:
                stale = queue.get_nowait()
            except asyncio.QueueEmpty:
                stale = b""
            dropped += 1
            queue.put_nowait(msg.data)
            # ACK THE DROPPED BYTES. The ack window counts what the agent
            # SENT, not what the browser displayed, so bytes we throw away are
            # still owed back. Without this the window leaks a little on every
            # drop, and once the leak passes the agent's 512 KiB pause
            # threshold the PTY stops being read for good: the terminal goes
            # permanently silent with no error anywhere, and only closing the
            # session clears it.
            if stale:
                await ack(len(stale))

    async def on_ctl(msg) -> None:
        nonlocal close_reason
        try:
            event = json.loads(msg.data)
        except json.JSONDecodeError:
            return
        if event.get("event") == "closed":
            close_reason = event.get("reason", "exit")
            agent_closed.set()
        elif event.get("event") == "gap":
            # A banner is cosmetic; never block the ctl callback for it.
            with contextlib.suppress(asyncio.QueueFull):
                queue.put_nowait(b"\r\n\x1b[33m[output gap: agent was disconnected]\x1b[0m\r\n")

    sub_out = await nc.subscribe(shell_out(agent_id, sid), cb=on_out)
    sub_ctl = await nc.subscribe(shell_ctl(agent_id, sid), cb=on_ctl)

    async def pump_to_browser() -> None:
        closed = asyncio.ensure_future(agent_closed.wait())
        try:
            while True:
                nxt = asyncio.ensure_future(queue.get())
                done, _ = await asyncio.wait({nxt, closed}, return_when=asyncio.FIRST_COMPLETED)
                if nxt not in done:
                    # PTY is gone. Drain what already arrived so the last of
                    # the command output reaches the browser instead of dying
                    # with the session.
                    nxt.cancel()
                    while not queue.empty():
                        await deliver(queue.get_nowait())
                    return
                await deliver(nxt.result())
        finally:
            closed.cancel()

    async def deliver(data: bytes) -> None:
        nonlocal bytes_out
        # awaited write => browser TCP backpressure reaches us here
        await websocket.send_bytes(data)
        bytes_out += len(data)
        if recorder is not None:
            recorder.write_output(data)
            await recorder.maybe_flush()
        # ack AFTER the write: this is the flow-control signal
        await ack(len(data))

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
                    try:
                        cols, rows = int(event["cols"]), int(event["rows"])
                    except (KeyError, TypeError, ValueError):
                        # A malformed resize is the browser's problem, not a
                        # reason to tear down a working shell.
                        continue
                    await nc.publish(
                        shell_resize(agent_id, sid),
                        json.dumps({"cols": cols, "rows": rows}).encode(),
                    )
                    if recorder is not None:
                        recorder.write_resize(cols, rows)

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
        # FIRST_COMPLETED, not FIRST_EXCEPTION. With no exception raised,
        # FIRST_EXCEPTION waits for EVERY task, and two of these never return
        # on their own: a clean PTY exit left the bridge hanging until the
        # browser happened to disconnect, holding a session, a websocket and
        # two subscriptions open for a shell that had already gone.
        done, _ = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
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
            await request_agent(
                nc, agent_id, "shell.close", json.dumps({"session_id": sid}).encode(), timeout=2
            )
        await sub_out.unsubscribe()
        await sub_ctl.unsubscribe()
        if recorder is not None:
            await recorder.aclose()
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
