from typing import Annotated

from fastapi import Cookie, Depends, HTTPException, status
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.db.deps import db_session
from everwas.models.user import Role, User
from everwas.security.sessions import SESSION_COOKIE, resolve_session

DbSession = Annotated[AsyncSession, Depends(db_session)]


async def current_user(
    db: DbSession,
    everwas_session: Annotated[str | None, Cookie(alias=SESSION_COOKIE)] = None,
) -> User:
    if not everwas_session:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "Not authenticated")
    user = await resolve_session(db, everwas_session)
    if user is None:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "Session expired")
    return user


CurrentUser = Annotated[User, Depends(current_user)]


def require_role(*roles: Role):
    async def check(user: CurrentUser) -> User:
        if user.role not in roles:
            raise HTTPException(status.HTTP_403_FORBIDDEN, "Insufficient role")
        return user

    return Depends(check)
