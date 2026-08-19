from typing import Annotated

from fastapi import APIRouter, Cookie, Form, HTTPException, Request, Response, status
from fastapi.responses import JSONResponse
from sqlalchemy import select

from everwas.api.deps import CurrentUser, DbSession
from everwas.config import get_settings
from everwas.models.audit import ActorType, AuditLog
from everwas.models.user import User
from everwas.schemas.auth import LoginRequest, UserOut
from everwas.security.api_keys import KEY_PREFIX, authenticate_key
from everwas.security.passwords import verify_password
from everwas.security.sessions import SESSION_COOKIE, create_session, destroy_session
from everwas.security.sync_tokens import TokenIssueError, issue

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
    everwas_session: Annotated[str | None, Cookie(alias=SESSION_COOKIE)] = None,
) -> None:
    if everwas_session:
        await destroy_session(db, everwas_session)
        await db.commit()
    response.delete_cookie(SESSION_COOKIE, path="/")


@router.get("/me")
async def me(user: CurrentUser) -> UserOut:
    return UserOut.model_validate(user)


def _oauth_error(code: int, error: str) -> JSONResponse:
    """RFC 6749 §5.2 error shape. 401s carry WWW-Authenticate as required."""
    headers = {"WWW-Authenticate": "Basic"} if code == 401 else None
    return JSONResponse({"error": error}, status_code=code, headers=headers)


@router.post("/token")
async def token(
    db: DbSession,
    grant_type: Annotated[str, Form()] = "",
    client_id: Annotated[str, Form()] = "",
    client_secret: Annotated[str, Form()] = "",
):
    """OAuth2 client-credentials for machine callers (the sync API).

    The credential is an existing scoped API key. Two spellings are accepted:
    the whole `ewpk_<id>_<secret>` as client_secret with client_id blank (the
    convenient one), or the key id as client_id and the secret alone as
    client_secret (the one OAuth2 client libraries produce). Either way the
    key is exchanged for a short-lived ewst_ bearer token; scopes ride along
    frozen. Refusals audit without naming which part failed, matching the
    indistinguishable-failure rule for the keys themselves.
    """
    if grant_type != "client_credentials":
        return _oauth_error(400, "unsupported_grant_type")

    raw_key = client_secret
    if client_id and not client_secret.startswith(KEY_PREFIX):
        raw_key = f"{KEY_PREFIX}{client_id}_{client_secret}"

    principal = await authenticate_key(db, raw_key)
    if principal is None:
        db.add(
            AuditLog(
                actor_type=ActorType.api_key,
                actor_id=(client_id[:64] or None),
                action="sync.token_refused",
            )
        )
        await db.commit()
        return _oauth_error(401, "invalid_client")

    try:
        access_token, expires_in = issue(principal)
    except TokenIssueError as exc:
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, str(exc)) from exc

    db.add(
        AuditLog(
            org_id=principal.org_id,
            actor_type=ActorType.api_key,
            actor_id=principal.name[:64],
            action="sync.token_issued",
            detail={"scopes": list(principal.scopes), "expires_in": expires_in},
        )
    )
    await db.commit()
    return {
        "access_token": access_token,
        "token_type": "Bearer",
        "expires_in": expires_in,
        "scope": " ".join(principal.scopes),
    }
