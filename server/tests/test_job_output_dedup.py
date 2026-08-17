"""Job output must survive a redelivery without duplicating itself.

JetStream is at-least-once and the dispatcher acks after the commit, so any
process death between the two redelivers a chunk that was already applied.
apply_job_output appended unconditionally and never read the `seq` the agent
puts in every chunk, so the second delivery appended the same block again.

For a script whose output is a report, or is parsed by something downstream, a
256 KiB block repeated mid-stream is worse than losing it: it looks like data.

consumers.py states "all ingest is idempotent, so a replay is safe". These make
that true for this handler rather than aspirational.
"""

import base64
import uuid
from datetime import UTC, datetime

import pytest

from openrmm.db.engine import get_sessionmaker
from openrmm.ingest.results import apply_job_output
from openrmm.models.device import Device, OsFamily
from openrmm.models.script import RunStatus, RunTrigger, Script, ScriptRun, ShellKind
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


def chunk(text: str, *, seq: int, stream: str = "stdout") -> dict:
    return {
        "stream": stream,
        "seq": seq,
        "data": base64.b64encode(text.encode()).decode(),
    }


async def _run() -> tuple[uuid.UUID, uuid.UUID]:
    async with get_sessionmaker()() as db, db.begin():
        device = Device(id=uuid7(), hostname="out-host", os_family=OsFamily.linux, tags=[])
        script = Script(
            name=f"s-{uuid.uuid4().hex[:6]}",
            shell=ShellKind.bash,
            body="echo hi",
            sha256="0" * 64,
            timeout_s=60,
        )
        db.add_all([device, script])
        await db.flush()
        run = ScriptRun(
            id=uuid7(),
            script_id=script.id,
            device_id=device.id,
            trigger=RunTrigger.manual,
            status=RunStatus.running,
            queued_at=datetime.now(UTC),
        )
        db.add(run)
        await db.flush()
        return device.id, run.id


async def _apply(device_id, run_id, payload):
    async with get_sessionmaker()() as db, db.begin():
        await apply_job_output(db, device_id, run_id, payload)


async def _stdout(run_id) -> str:
    async with get_sessionmaker()() as db:
        return (await db.get(ScriptRun, run_id)).stdout


async def test_a_redelivered_chunk_is_applied_once():
    device_id, run_id = await _run()
    await _apply(device_id, run_id, chunk("FAIL: non-compliant\n", seq=0))
    # Same message, delivered again after an ack was lost.
    await _apply(device_id, run_id, chunk("FAIL: non-compliant\n", seq=0))

    assert await _stdout(run_id) == "FAIL: non-compliant\n"


async def test_successive_chunks_still_accumulate():
    device_id, run_id = await _run()
    await _apply(device_id, run_id, chunk("one\n", seq=0))
    await _apply(device_id, run_id, chunk("two\n", seq=1))
    await _apply(device_id, run_id, chunk("three\n", seq=2))

    assert await _stdout(run_id) == "one\ntwo\nthree\n"


async def test_the_two_streams_have_independent_sequences():
    # The agent numbers stdout and stderr from one counter, but they land in
    # two columns. Tracking one high-water mark for both would drop every
    # interleaved chunk of whichever stream fell behind.
    device_id, run_id = await _run()
    await _apply(device_id, run_id, chunk("out0\n", seq=0, stream="stdout"))
    await _apply(device_id, run_id, chunk("err1\n", seq=1, stream="stderr"))
    await _apply(device_id, run_id, chunk("out2\n", seq=2, stream="stdout"))

    async with get_sessionmaker()() as db:
        run = await db.get(ScriptRun, run_id)
    assert run.stdout == "out0\nout2\n"
    assert run.stderr == "err1\n"


async def test_a_chunk_with_no_sequence_is_still_applied():
    # Older agents, and any publisher that omits seq. Dropping these would be
    # worse than the duplicate this guards against.
    device_id, run_id = await _run()
    await _apply(device_id, run_id, {"stream": "stdout", "data": base64.b64encode(b"x").decode()})
    assert await _stdout(run_id) == "x"


async def test_output_for_an_unknown_run_is_logged_not_silently_dropped(capsys):
    # apply_job_result logs "result for unknown run"; this path returned in
    # silence, so every patch job's output was discarded without a trace.
    #
    # capsys rather than caplog: structlog is configured to write to stdout
    # directly, so the stdlib logging capture sees nothing.
    device_id, _ = await _run()
    await _apply(device_id, uuid7(), chunk("orphan\n", seq=0))
    out = capsys.readouterr().out
    assert "unknown run" in out.lower(), f"nothing was logged; stdout was: {out!r}"
