"""Authentication for /api/v1/sync: bearer tokens only, scopes not roles.

A second authentication root, deliberately disjoint from the SPA's session
cookie. A sync caller cannot ride a browser session and a browser cannot
wander into the sync surface, so neither's compromise story includes the
other. tests/test_smoke.py knows sync_principal as a guard the same way it
knows current_user.
"""

from typing import Annotated

from fastapi import Depends, Header, HTTPException, status

from everwas.api.deps import DbSession
from everwas.security.api_keys import KEY_PREFIX, ApiKeyPrincipal
from everwas.security.sync_tokens import TOKEN_PREFIX, verify


async def sync_principal(
    db: DbSession,
    authorization: Annotated[str | None, Header()] = None,
) -> ApiKeyPrincipal:
    if not authorization or not authorization.lower().startswith("bearer "):
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "Not authenticated. Present a sync token: Authorization: Bearer ewst_...",
            headers={"WWW-Authenticate": "Bearer"},
        )
    token = authorization[len("bearer ") :].strip()
    if token.startswith(KEY_PREFIX):
        # A raw API key is the credential for MINTING tokens, not for calling
        # the sync surface. Saying so beats a bare 401 the first time an
        # integration is wired up.
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "This is an API key, not a sync token. Exchange it first: "
            "POST /api/v1/auth/token with grant_type=client_credentials.",
            headers={"WWW-Authenticate": "Bearer"},
        )
    principal = await verify(db, token) if token.startswith(TOKEN_PREFIX) else None
    if principal is None:
        raise HTTPException(
            status.HTTP_401_UNAUTHORIZED,
            "Invalid or expired sync token.",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return principal


SyncPrincipal = Annotated[ApiKeyPrincipal, Depends(sync_principal)]


def require_sync_scope(scope: str):
    """Route dependency: 403 with the held scopes named, mirroring the MCP
    refusal message so the fix (reissue the key with the scope) is obvious."""

    async def check(principal: SyncPrincipal) -> ApiKeyPrincipal:
        if not principal.has(scope):
            held = ", ".join(principal.scopes) or "none"
            raise HTTPException(
                status.HTTP_403_FORBIDDEN,
                f"This token lacks the '{scope}' scope. Scopes on this token: {held}.",
            )
        return principal

    return Depends(check)
