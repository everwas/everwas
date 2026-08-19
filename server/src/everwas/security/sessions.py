import hashlib
import secrets
from datetime import UTC, datetime, timedelta

from sqlalchemy import delete, select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.config import get_settings
from everwas.models.session import Session
from everwas.models.user import User

SESSION_COOKIE = "everwas_session"


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


async def create_session(
    db: AsyncSession, user: User, ip: str | None, user_agent: str | None
) -> str:
    token = secrets.token_urlsafe(32)
    ttl = timedelta(hours=get_settings().session_ttl_hours)
    db.add(
        Session(
            token_hash=_hash(token),
            user_id=user.id,
            expires_at=datetime.now(UTC) + ttl,
            ip=ip,
            user_agent=(user_agent or "")[:255] or None,
        )
    )
    return token


async def resolve_session(db: AsyncSession, token: str) -> User | None:
    row = await db.execute(
        select(User)
        .join(Session, Session.user_id == User.id)
        .where(Session.token_hash == _hash(token), Session.expires_at > datetime.now(UTC))
    )
    user = row.scalar_one_or_none()
    return user if user is not None and user.is_active else None


async def destroy_session(db: AsyncSession, token: str) -> None:
    await db.execute(delete(Session).where(Session.token_hash == _hash(token)))
