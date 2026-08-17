import asyncio
import contextlib
import uuid
from pathlib import Path

import structlog
from fastapi import APIRouter, HTTPException, Query, WebSocket, status
from fastapi.responses import FileResponse
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession, require_role
from openrmm.config import get_settings
from openrmm.db.engine import session_scope
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device
from openrmm.models.script import ShellSession
from openrmm.models.user import Role
from openrmm.natsio.client import get_nats
from openrmm.security.sessions import SESSION_COOKIE, resolve_session
from openrmm.services.shell_session import (
    bridge_shell,
    close_session_record,
    open_session_record,
)

log = structlog.get_logger()
router = APIRouter()

# Reading a session back is as sensitive as opening one, so this matches the
# role check the WebSocket handler does inline.
OPERATOR = require_role(Role.admin, Role.operator)


@router.websocket("/{device_id}/shell")
async def device_shell(
    websocket: WebSocket,
    device_id: uuid.UUID,
    cols: int = Query(default=80, ge=20, le=500),
    rows: int = Query(default=24, ge=5, le=200),
) -> None:
    # WebSocket auth: same session cookie, checked before accept.
    token = websocket.cookies.get(SESSION_COOKIE)
    if not token:
        await websocket.close(code=status.WS_1008_POLICY_VIOLATION)
        return
    async with session_scope() as db:
        user = await resolve_session(db, token)
        if user is None or user.role not in (Role.admin, Role.operator):
            await websocket.close(code=status.WS_1008_POLICY_VIOLATION)
            return
        device = (
            await db.execute(select(Device).where(Device.id == device_id))
        ).scalar_one_or_none()
        if device is None:
            await websocket.close(code=status.WS_1008_POLICY_VIOLATION)
            return
        user_id, user_email = user.id, user.email

    await websocket.accept()

    settings = get_settings()
    record_dir = Path(settings.recordings_dir)

    # The record exists BEFORE any bytes flow. Everything below this point can
    # fail, including by the process disappearing, and a root shell that
    # happened must not depend on this handler surviving to say so.
    session_id = uuid.uuid4()
    await open_session_record(session_id, device_id, user_id=user_id, user_email=user_email)

    close_reason, bytes_in, bytes_out = "error", 0, 0
    try:
        _, close_reason, bytes_in, bytes_out = await bridge_shell(
            websocket,
            get_nats(),
            device_id,
            requested_by=user_email,
            cols=cols,
            rows=rows,
            record_dir=record_dir,
            session_id=session_id,
        )
    except Exception as exc:
        log.warning("shell session failed", device_id=str(device_id), error=str(exc))
        message = str(exc)[:120]
        with contextlib.suppress(Exception):
            await websocket.send_text(f"\r\n\x1b[31mshell error: {message}\x1b[0m\r\n")

    await close_session_record(
        session_id,
        close_reason=close_reason,
        bytes_in=bytes_in,
        bytes_out=bytes_out,
    )
    async with session_scope() as db:
        db.add(
            AuditLog(
                actor_type=ActorType.user,
                actor_id=user_email,
                action="shell.session",
                target_type="device",
                target_id=str(device_id),
                detail={
                    "session_id": str(session_id),
                    "reason": close_reason,
                    "bytes_in": bytes_in,
                    "bytes_out": bytes_out,
                },
            )
        )

    with contextlib.suppress(Exception):
        await websocket.close(code=status.WS_1000_NORMAL_CLOSURE)


@router.get("/sessions/recent", dependencies=[OPERATOR])
async def recent_sessions(
    db: DbSession,
    _user: CurrentUser,
    device_id: uuid.UUID | None = None,
) -> list[dict]:
    """Who has had a root shell on which machine, and when.

    Gated on operator, matching the WebSocket that opens a session: a recording
    is a verbatim transcript of a root shell, including every credential pasted
    into it, so read-back has to be at least as restricted as the act. It was
    first missing authentication entirely (anonymous callers could enumerate
    device UUIDs and read admin session history), then had authentication but no
    role, which let a viewer download an admin's session.
    """
    stmt = select(ShellSession).order_by(ShellSession.started_at.desc()).limit(50)
    if device_id is not None:
        stmt = stmt.where(ShellSession.device_id == device_id)
    rows = await db.execute(stmt)
    return [
        {
            "id": str(s.id),
            "device_id": str(s.device_id),
            "started_at": s.started_at.isoformat(),
            "ended_at": s.ended_at.isoformat() if s.ended_at else None,
            "close_reason": s.close_reason,
            "recording_path": s.recording_path,
            "bytes_out": s.bytes_out,
        }
        for s in rows.scalars()
    ]


@router.get("/sessions/{session_id}/recording", dependencies=[OPERATOR])
async def session_recording(
    session_id: uuid.UUID, db: DbSession, _user: CurrentUser
) -> FileResponse:
    """Serve one asciicast v2 recording.

    Every session has been recorded since M3 and nothing has ever been able to
    read one back, so the files accumulated as evidence that could only be
    found by somebody with shell access to the server.

    The path is rebuilt from the session id rather than taken from the stored
    `recording_path`. That column is written by the bridge and read here, and
    a value that ever became attacker-influenced would be a straight path
    traversal out of the recordings directory. Deriving it means the only
    input is a UUID that FastAPI has already parsed.
    """
    session = await db.get(ShellSession, session_id)
    if session is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown session")
    if session.recording_path is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "this session was not recorded")

    def _resolve() -> Path | None:
        root = Path(get_settings().recordings_dir).resolve()
        path = (root / f"{session_id}.cast").resolve()
        # is_relative_to is belt and braces given the id is a parsed UUID, but
        # the check costs nothing and the failure it prevents is reading any
        # file on the box.
        return path if path.is_relative_to(root) and path.is_file() else None

    # Off-thread. A stat and a read are blocking syscalls, and doing them on
    # the loop stalls every other request this worker is serving; that is the
    # same mistake the asciicast recorder made on the write side.
    path = await asyncio.to_thread(_resolve)
    if path is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "recording is missing")

    log.info("recording served", session_id=str(session_id), actor=_user.email)
    # FileResponse streams through anyio rather than reading the whole file
    # into memory, which matters because a long session is megabytes.
    return FileResponse(
        path,
        media_type="application/x-asciicast",
        filename=f"{session_id}.cast",
        content_disposition_type="inline",
    )
