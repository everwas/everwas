"""A root shell must be on the record before it starts, not after it ends.

Both the ShellSession row and the audit entry were written after bridge_shell
returned. Three consequences:

Auditing could be skipped by a process death. A worker OOM, a container restart,
a `docker compose up -d` during a session, or an exception raised outside the
try block, and there was no row anywhere naming who had root on which machine.
The .cast file sat on disk with its bytes intact and nothing pointing at it, and
the download endpoint only resolves paths for sessions it can look up, so the
evidence was reachable only by someone with shell access to the server. That is
precisely the situation the module docstring says it exists to eliminate.

No session was ever visible while it was live, so "who has a root shell open
right now" was unanswerable.

And started_at was server_default=now(), evaluated at INSERT, which was session
END. So started_at was approximately equal to ended_at and every root shell in
the audit trail had a duration of zero, while recent_sessions ordered by it.
"""

import uuid
from datetime import UTC, datetime

import pytest
from sqlalchemy import select

from openrmm.db.engine import get_sessionmaker
from openrmm.models.audit import AuditLog
from openrmm.models.device import Device, OsFamily
from openrmm.models.script import ShellSession
from openrmm.services.shell_session import close_session_record, open_session_record
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _device() -> uuid.UUID:
    async with get_sessionmaker()() as db, db.begin():
        d = Device(id=uuid7(), hostname="shell-rec", os_family=OsFamily.linux, tags=[])
        db.add(d)
        await db.flush()
        return d.id


async def _row(session_id) -> ShellSession | None:
    async with get_sessionmaker()() as db:
        return await db.get(ShellSession, session_id)


async def test_the_session_exists_before_any_bytes_flow():
    device_id = await _device()
    session_id = uuid.uuid4()

    await open_session_record(session_id, device_id, user_id=None, user_email="admin@example.com")

    row = await _row(session_id)
    assert row is not None, "a root shell was open with no record of it anywhere"
    assert row.ended_at is None, "an open session must be distinguishable from a closed one"
    assert row.recording_path == f"{session_id}.cast"


async def test_the_audit_entry_is_written_at_open_not_at_close():
    device_id = await _device()
    session_id = uuid.uuid4()

    await open_session_record(session_id, device_id, user_id=None, user_email="admin@example.com")

    async with get_sessionmaker()() as db:
        rows = (
            (await db.execute(select(AuditLog).where(AuditLog.action == "shell.opened")))
            .scalars()
            .all()
        )
    assert any(r.target_id == str(device_id) for r in rows), (
        "no audit entry until the session ended, so a crash mid-session erased "
        "the only record that someone had root on this machine"
    )


async def test_started_at_is_the_start_not_the_end():
    device_id = await _device()
    session_id = uuid.uuid4()
    before = datetime.now(UTC)

    await open_session_record(session_id, device_id, user_id=None, user_email="admin@example.com")
    row = await _row(session_id)
    assert row.started_at >= before.replace(microsecond=0) - __import__("datetime").timedelta(
        seconds=5
    )

    await close_session_record(session_id, close_reason="exit", bytes_in=10, bytes_out=200)
    row = await _row(session_id)
    assert row.ended_at is not None
    assert row.ended_at >= row.started_at
    assert row.close_reason == "exit"
    assert row.bytes_out == 200


async def test_an_unclosed_session_is_self_describing():
    """A session whose process died has ended_at NULL and stays that way.

    That is the point: a row with no ended_at older than the idle timeout is a
    session nobody closed cleanly, which is a question an operator can ask.
    Writing nothing at all was not.
    """
    device_id = await _device()
    session_id = uuid.uuid4()
    await open_session_record(session_id, device_id, user_id=None, user_email="admin@example.com")
    # No close: the worker died here.
    row = await _row(session_id)
    assert row is not None
    assert row.ended_at is None
    assert row.close_reason is None
