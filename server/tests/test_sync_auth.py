"""The sync surface's authentication: key -> token -> bearer call.

The matrix pins both halves of the exchange (POST /api/v1/auth/token) and
the verification path (sync_principal), including the property the design
paid for: revoking an API key kills its outstanding tokens immediately,
not at token expiry.
"""

import hashlib
import secrets
import uuid
from datetime import UTC, datetime, timedelta

import httpx
import pytest
from sqlalchemy import delete

from openrmm.db.engine import get_sessionmaker, session_scope
from openrmm.models.api_key import ApiKey
from openrmm.security import sync_tokens
from openrmm.security.api_keys import authenticate_key

pytestmark = pytest.mark.usefixtures("pg_database")


async def mint_key(
    name: str, scopes: list[str], *, expires_in: timedelta | None = None
) -> tuple[uuid.UUID, str]:
    key_id = secrets.token_hex(11)
    secret = secrets.token_urlsafe(32)
    async with get_sessionmaker()() as db, db.begin():
        key = ApiKey(
            name=name,
            key_id=key_id,
            secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
            scopes=scopes,
            expires_at=(datetime.now(UTC) + expires_in if expires_in else None),
        )
        db.add(key)
        await db.flush()
        row_id = key.id
    return row_id, f"orpk_{key_id}_{secret}"


def sync_app():
    """The real app plus one scratch route guarded by the sync dependency,
    so the dependency's behavior is pinned before the sync router lands."""
    from openrmm.api.app import create_app
    from openrmm.api.sync_deps import SyncPrincipal, require_sync_scope

    app = create_app()

    async def whoami(principal: SyncPrincipal) -> dict:
        return {"name": principal.name, "scopes": list(principal.scopes)}

    async def guarded(principal: SyncPrincipal) -> dict:  # pragma: no cover - body unused
        return {"ok": True}

    app.add_api_route("/test-sync/whoami", whoami, methods=["GET"])
    app.add_api_route(
        "/test-sync/needs-patches",
        guarded,
        methods=["GET"],
        dependencies=[require_sync_scope("patches:read")],
    )
    return app


def client_for(app) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test")


async def exchange(c: httpx.AsyncClient, **form) -> httpx.Response:
    return await c.post("/api/v1/auth/token", data={"grant_type": "client_credentials", **form})


# --- the token exchange ----------------------------------------------------


async def test_whole_key_as_client_secret():
    _, raw = await mint_key("nautobot", ["devices:read"])
    async with client_for(sync_app()) as c:
        resp = await exchange(c, client_secret=raw)
    assert resp.status_code == 200
    body = resp.json()
    assert body["access_token"].startswith("orst_")
    assert body["token_type"] == "Bearer"
    assert body["scope"] == "devices:read"
    assert body["expires_in"] > 0


async def test_split_client_id_and_secret():
    _, raw = await mint_key("split", ["devices:read"])
    _, key_id, secret = raw.split("_", 2)
    async with client_for(sync_app()) as c:
        resp = await exchange(c, client_id=key_id, client_secret=secret)
    assert resp.status_code == 200


async def test_wrong_grant_type():
    async with client_for(sync_app()) as c:
        resp = await c.post("/api/v1/auth/token", data={"grant_type": "password"})
    assert resp.status_code == 400
    assert resp.json()["error"] == "unsupported_grant_type"


async def test_wrong_secret_is_invalid_client():
    _, raw = await mint_key("victim", ["devices:read"])
    async with client_for(sync_app()) as c:
        resp = await exchange(c, client_secret=raw[:-4] + "XXXX")
    assert resp.status_code == 401
    assert resp.json()["error"] == "invalid_client"
    assert "WWW-Authenticate" in resp.headers


async def test_expired_key_cannot_exchange():
    _, raw = await mint_key("stale", ["devices:read"], expires_in=timedelta(seconds=-1))
    async with client_for(sync_app()) as c:
        resp = await exchange(c, client_secret=raw)
    assert resp.status_code == 401


# --- bearer verification ---------------------------------------------------


async def bearer_token(scopes: list[str]) -> tuple[uuid.UUID, str]:
    row_id, raw = await mint_key("bearer", scopes)
    async with session_scope() as db:
        principal = await authenticate_key(db, raw)
    token, _ = sync_tokens.issue(principal)
    return row_id, token


async def test_valid_token_authenticates():
    _, token = await bearer_token(["devices:read"])
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 200
    assert resp.json()["name"] == "bearer"


async def test_no_header_is_401():
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami")
    assert resp.status_code == 401


async def test_raw_api_key_is_refused_with_directions():
    _, raw = await mint_key("misused", ["devices:read"])
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {raw}"})
    assert resp.status_code == 401
    assert "/api/v1/auth/token" in resp.json()["detail"]


async def test_session_cookie_does_not_reach_sync():
    """The two authentication roots stay disjoint: the sync dependency never
    consults the cookie the SPA rides."""
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami", cookies={"openrmm_session": "anything"})
    assert resp.status_code == 401


async def test_tampered_signature_is_401():
    _, token = await bearer_token(["devices:read"])
    claims_seg, sig = token.removeprefix("orst_").split(".")
    forged = f"orst_{claims_seg}.{sig[:-4]}AAAA"
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {forged}"})
    assert resp.status_code == 401


async def test_expired_token_is_401():
    # Rewrite a real token's exp into the past and re-sign it with the real
    # key: the signature is valid, only time has run out.
    _, token = await bearer_token(["devices:read"])
    import base64
    import json

    claims_seg, _ = token.removeprefix("orst_").split(".")
    claims = json.loads(base64.urlsafe_b64decode(claims_seg + "=" * (-len(claims_seg) % 4)))
    claims["exp"] = claims["iat"] - 1
    stale_bytes = json.dumps(claims, separators=(",", ":"), sort_keys=True).encode()
    stale = (
        "orst_"
        + base64.urlsafe_b64encode(stale_bytes).decode().rstrip("=")
        + "."
        + base64.urlsafe_b64encode(sync_tokens._sign(stale_bytes)).decode().rstrip("=")
    )
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {stale}"})
    assert resp.status_code == 401


async def test_revoking_key_kills_outstanding_tokens_now():
    row_id, token = await bearer_token(["devices:read"])
    app = sync_app()
    async with client_for(app) as c:
        ok = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {token}"})
        assert ok.status_code == 200
        async with get_sessionmaker()() as db, db.begin():
            await db.execute(delete(ApiKey).where(ApiKey.id == row_id))
        refused = await c.get("/test-sync/whoami", headers={"Authorization": f"Bearer {token}"})
    assert refused.status_code == 401


async def test_missing_scope_is_403_and_names_held_scopes():
    _, token = await bearer_token(["devices:read"])
    async with client_for(sync_app()) as c:
        resp = await c.get("/test-sync/needs-patches", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 403
    assert "patches:read" in resp.json()["detail"]
    assert "devices:read" in resp.json()["detail"]
