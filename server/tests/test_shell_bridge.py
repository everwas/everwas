"""The shell bridge's flow control and teardown.

Three defects lived here, all of which look like "the terminal just stopped"
to whoever was using it:

  * dropped output frames were never acked, so the agent's byte window leaked
    until it passed the pause threshold and the PTY stopped being read for
    good;
  * a clean PTY exit did not end the bridge, because asyncio.wait with
    FIRST_EXCEPTION waits for every task when nothing raises and two of them
    never return;
  * asciicast recording wrote to disk synchronously on the event loop, once
    per frame.

These drive the real bridge with fakes for the websocket and NATS.
"""

import asyncio
import json
import uuid

import pytest

from openrmm.services import shell_session
from openrmm.services.shell_session import QUEUE_MAXSIZE, AsciicastRecorder, bridge_shell


class FakeNats:
    """Records publishes and hands out the subscription callbacks."""

    def __init__(self) -> None:
        self.published: list[tuple[str, bytes]] = []
        self.callbacks: dict[str, object] = {}

    async def publish(self, subject: str, payload: bytes) -> None:
        self.published.append((subject, payload))

    async def subscribe(self, subject: str, cb=None):
        self.callbacks[subject] = cb
        return FakeSub()

    def acked_bytes(self) -> int:
        total = 0
        for _subject, payload in self.published:
            with __import__("contextlib").suppress(Exception):
                total += int(json.loads(payload).get("ack", 0))
        return total

    def cb_for(self, fragment: str):
        for subject, cb in self.callbacks.items():
            if subject.endswith(fragment):
                return cb
        raise AssertionError(f"no subscription ending in {fragment}: {list(self.callbacks)}")


class FakeSub:
    async def unsubscribe(self) -> None:
        pass


class FakeWebSocket:
    """A browser that accepts everything and never sends anything."""

    def __init__(self) -> None:
        self.sent: list[bytes] = []

    async def send_bytes(self, data: bytes) -> None:
        self.sent.append(data)

    async def receive(self) -> dict:
        await asyncio.sleep(3600)  # a real browser just sits there
        raise AssertionError("unreachable")


@pytest.fixture
def accepted(monkeypatch):
    """Make shell.open succeed and shell.close a no-op."""

    async def fake_request(nc, agent_id, op, payload, *, timeout=10):  # noqa: ASYNC109
        return json.dumps({"accepted": True}).encode()

    monkeypatch.setattr(shell_session, "request_agent", fake_request)


async def _run_bridge(nc, ws):
    task = asyncio.create_task(bridge_shell(ws, nc, uuid.uuid4(), requested_by="admin@example.com"))
    # Let the bridge get through shell.open and subscribe.
    for _ in range(50):
        await asyncio.sleep(0)
        if len(nc.callbacks) >= 2:
            break
    assert len(nc.callbacks) >= 2, "bridge never subscribed"
    return task


class Msg:
    def __init__(self, data: bytes) -> None:
        self.data = data


async def test_a_clean_pty_exit_ends_the_bridge(accepted):
    """The agent said the shell is gone. The bridge has to notice.

    With FIRST_EXCEPTION and no exception raised, asyncio.wait waits for ALL
    tasks, and the browser pump and the pinger never return: the session, the
    websocket and both subscriptions stayed open until the browser happened to
    disconnect.
    """
    nc, ws = FakeNats(), FakeWebSocket()
    task = await _run_bridge(nc, ws)

    await nc.cb_for(".ctl")(Msg(json.dumps({"event": "closed", "reason": "exit"}).encode()))

    _session_id, reason, _bin, _bout = await asyncio.wait_for(task, timeout=2)
    assert reason == "exit"


async def test_output_still_queued_at_close_is_delivered(accepted):
    """A command's last line must not die with the shell that printed it."""
    nc, ws = FakeNats(), FakeWebSocket()
    task = await _run_bridge(nc, ws)

    on_out = nc.cb_for(".out")
    await on_out(Msg(b"the answer is 42\r\n"))
    await nc.cb_for(".ctl")(Msg(json.dumps({"event": "closed"}).encode()))

    await asyncio.wait_for(task, timeout=2)
    assert b"the answer is 42\r\n" in ws.sent


async def test_dropped_frames_are_still_acked(accepted):
    """The leak that silently killed sessions.

    The ack window counts what the agent SENT, not what the browser displayed.
    A frame the bridge drops is still owed back, and without that the window
    leaks on every drop until it passes the agent's 512 KiB pause threshold,
    at which point the PTY stops being read permanently.
    """
    nc, ws = FakeNats(), FakeWebSocket()
    task = await _run_bridge(nc, ws)

    on_out = nc.cb_for(".out")
    frame = b"x" * 1024
    # Overfill hard. The pump is not being scheduled between these calls, so
    # the queue fills and the drop path runs.
    sent = 0
    for _ in range(QUEUE_MAXSIZE * 3):
        await on_out(Msg(frame))
        sent += len(frame)

    await nc.cb_for(".ctl")(Msg(json.dumps({"event": "closed"}).encode()))
    await asyncio.wait_for(task, timeout=5)

    assert nc.acked_bytes() == sent, (
        f"acked {nc.acked_bytes()} of {sent} bytes the agent sent; the "
        f"{sent - nc.acked_bytes()} byte shortfall is a permanent leak in the "
        "agent's flow-control window"
    )


async def test_every_delivered_byte_is_acked(accepted):
    """The ordinary path, so the compensating ack cannot double-count."""
    nc, ws = FakeNats(), FakeWebSocket()
    task = await _run_bridge(nc, ws)

    on_out = nc.cb_for(".out")
    for i in range(5):
        await on_out(Msg(f"line {i}\r\n".encode()))
        await asyncio.sleep(0)  # let the pump drain each one

    await nc.cb_for(".ctl")(Msg(json.dumps({"event": "closed"}).encode()))
    await asyncio.wait_for(task, timeout=2)

    assert nc.acked_bytes() == sum(len(f) for f in ws.sent)


async def test_a_gap_banner_never_blocks_the_ctl_callback(accepted):
    """on_ctl runs on the NATS callback goroutine. Blocking it on a full queue
    stalls every control message behind it, including the close."""
    nc, ws = FakeNats(), FakeWebSocket()
    task = await _run_bridge(nc, ws)

    on_out, on_ctl = nc.cb_for(".out"), nc.cb_for(".ctl")
    for _ in range(QUEUE_MAXSIZE * 2):
        await on_out(Msg(b"y" * 512))

    await asyncio.wait_for(on_ctl(Msg(json.dumps({"event": "gap"}).encode())), timeout=1)
    await on_ctl(Msg(json.dumps({"event": "closed"}).encode()))
    await asyncio.wait_for(task, timeout=5)


async def test_recorder_does_not_write_from_the_calling_thread(tmp_path):
    """write_output must be pure buffering. It runs in the output pump, and a
    synchronous disk write there stalls every other connection this worker is
    serving."""
    rec = AsciicastRecorder(tmp_path / "s.cast", 80, 24)
    for _ in range(10):
        rec.write_output(b"hello")

    # Nothing has been flushed: the header and every event are still in memory.
    assert (tmp_path / "s.cast").read_text() == ""

    await rec.aclose()
    lines = (tmp_path / "s.cast").read_text().splitlines()
    assert json.loads(lines[0])["version"] == 2
    assert len(lines) == 11
    assert json.loads(lines[1])[1] == "o"


async def test_recorder_flushes_when_the_buffer_grows(tmp_path):
    """Otherwise a long session holds the whole recording in memory."""
    rec = AsciicastRecorder(tmp_path / "s.cast", 80, 24)
    while rec._pending_bytes < AsciicastRecorder.FLUSH_BYTES:
        rec.write_output(b"z" * 1024)
    await rec.maybe_flush()

    assert (tmp_path / "s.cast").read_text() != ""
    await rec.aclose()
