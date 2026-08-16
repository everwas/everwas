import contextlib
import uuid
from datetime import UTC, datetime
from pathlib import Path

import structlog
from fastapi import APIRouter, Query, WebSocket, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession
from openrmm.config import get_settings
from openrmm.db.engine import session_scope
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device
from openrmm.models.script import ShellSession
from openrmm.models.user import Role
from openrmm.natsio.client import get_nats
from openrmm.security.sessions import SESSION_COOKIE, resolve_session
from openrmm.services.shell_session import bridge_shell

log = structlog.get_logger()
router = APIRouter()


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
    session_id = None
    try:
        session_id, close_reason, bytes_in, bytes_out = await bridge_shell(
            websocket,
            get_nats(),
            device_id,
            requested_by=user_email,
            cols=cols,
            rows=rows,
            record_dir=record_dir,
        )
    except Exception as exc:
        log.warning("shell session failed", device_id=str(device_id), error=str(exc))
        message = str(exc)[:120]
        with contextlib.suppress(Exception):
            await websocket.send_text(f"\r\n\x1b[31mshell error: {message}\x1b[0m\r\n")
        close_reason, bytes_in, bytes_out = "error", 0, 0

    async with session_scope() as db:
        db.add(
            ShellSession(
                id=session_id or uuid.uuid4(),
                device_id=device_id,
                user_id=user_id,
                ended_at=datetime.now(UTC),
                close_reason=close_reason,
                recording_path=f"{session_id}.cast" if session_id else None,
                bytes_in=bytes_in,
                bytes_out=bytes_out,
            )
        )
        db.add(
            AuditLog(
                actor_type=ActorType.user,
                actor_id=user_email,
                action="shell.session",
                target_type="device",
                target_id=str(device_id),
                detail={
                    "session_id": str(session_id) if session_id else None,
                    "reason": close_reason,
                    "bytes_in": bytes_in,
                    "bytes_out": bytes_out,
                },
            )
        )

    with contextlib.suppress(Exception):
        await websocket.close(code=status.WS_1000_NORMAL_CLOSURE)


@router.get("/sessions/recent")
async def recent_sessions(
    db: DbSession,
    _user: CurrentUser,
    device_id: uuid.UUID | None = None,
) -> list[dict]:
    """Who has had a root shell on which machine, and when.

    This was the one route in the API with no authentication dependency, so
    an anonymous caller could enumerate device UUIDs and read admin session
    history. Every other read route takes CurrentUser; this one now does too.
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
