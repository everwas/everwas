from typing import Annotated

from fastapi import APIRouter, Cookie, HTTPException, Request, Response, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession
from openrmm.config import get_settings
from openrmm.models.user import User
from openrmm.schemas.auth import LoginRequest, UserOut
from openrmm.security.passwords import verify_password
from openrmm.security.sessions import SESSION_COOKIE, create_session, destroy_session

router = APIRouter()


@router.post("/login")
async def login(body: LoginRequest, request: Request, response: Response, db: DbSession) -> UserOut:
    row = await db.execute(select(User).where(User.email == body.email.lower()))
    user = row.scalar_one_or_none()
    if user is None or not user.is_active or not verify_password(body.password, user.password_hash):
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "Invalid credentials")
    token = await create_session(
        db,
        user,
        ip=request.client.host if request.client else None,
        user_agent=request.headers.get("user-agent"),
    )
    await db.commit()
    response.set_cookie(
        SESSION_COOKIE,
        token,
        httponly=True,
        secure=get_settings().mode == "prod",
        samesite="lax",
        max_age=get_settings().session_ttl_hours * 3600,
        path="/",
    )
    return UserOut.model_validate(user)


@router.post("/logout", status_code=status.HTTP_204_NO_CONTENT)
async def logout(
    response: Response,
    db: DbSession,
    openrmm_session: Annotated[str | None, Cookie(alias=SESSION_COOKIE)] = None,
) -> None:
    if openrmm_session:
        await destroy_session(db, openrmm_session)
        await db.commit()
    response.delete_cookie(SESSION_COOKIE, path="/")


@router.get("/me")
async def me(user: CurrentUser) -> UserOut:
    return UserOut.model_validate(user)
