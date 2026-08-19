"""Short-lived bearer tokens for the sync API: `ewst_<claims>.<signature>`.

An unattended integration should not hold a session cookie minted from a
human's password, and presenting the long-lived API key on every request
maximizes the window a leaked capture is useful. So the key is exchanged
(RFC 6749 client-credentials, POST /api/v1/auth/token) for a token that
expires on its own.

Deliberately not JWT: the only party that ever verifies these is the server
that signed them, so RFC-compliant JOSE headers, algorithm agility, and a
dependency buy nothing. Claims JSON, HMAC-SHA256 over those exact bytes with
the server's secret_key, both segments base64url. Stateless — no table, no
cleanup job — except that verification confirms the underlying key row still
exists and is unexpired: one indexed PK lookup on a session the request
already holds, and revoking a key kills its outstanding tokens immediately
instead of after the TTL.

Scopes are frozen at issuance. Narrowing a key's scopes takes effect for new
tokens; revoking the key takes effect now.
"""

import base64
import binascii
import hashlib
import hmac
import json
import uuid
from datetime import UTC, datetime

from sqlalchemy.ext.asyncio import AsyncSession

from everwas.config import get_settings
from everwas.models.api_key import ApiKey
from everwas.security.api_keys import ApiKeyPrincipal

TOKEN_PREFIX = "ewst_"

#: The Settings default. Signing bearer credentials with a value that is in
#: the public repository is not signing at all, so prod refuses.
_INSECURE_DEFAULT = "dev-only-insecure"


class TokenIssueError(RuntimeError):
    """The server is not in a state where tokens may be issued."""


def _b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode().rstrip("=")


def _unb64(seg: str) -> bytes:
    return base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4))


def _sign(claims_bytes: bytes) -> bytes:
    return hmac.new(get_settings().secret_key.encode(), claims_bytes, hashlib.sha256).digest()


def issue(principal: ApiKeyPrincipal) -> tuple[str, int]:
    """Mint a token for a verified principal. Returns (token, expires_in)."""
    settings = get_settings()
    if settings.mode == "prod" and settings.secret_key == _INSECURE_DEFAULT:
        raise TokenIssueError(
            "refusing to sign sync tokens with the default secret_key in prod; "
            "set EVERWAS_SECRET_KEY"
        )

    ttl = settings.sync_token_ttl_s
    now = int(datetime.now(UTC).timestamp())
    claims = {
        "kid": principal.id,
        "name": principal.name,
        "org": str(principal.org_id) if principal.org_id else None,
        "scopes": list(principal.scopes),
        "iat": now,
        "exp": now + ttl,
    }
    claims_bytes = json.dumps(claims, separators=(",", ":"), sort_keys=True).encode()
    token = f"{TOKEN_PREFIX}{_b64(claims_bytes)}.{_b64(_sign(claims_bytes))}"
    return token, ttl


async def verify(db: AsyncSession, token: str) -> ApiKeyPrincipal | None:
    """Resolve a bearer token to a principal, or None for any failure.

    As with raw keys, failures are indistinguishable: malformed, tampered,
    expired, and revoked all return None.
    """
    if not token.startswith(TOKEN_PREFIX):
        return None
    body = token[len(TOKEN_PREFIX) :]
    claims_seg, sep, sig_seg = body.partition(".")
    if not sep:
        return None
    try:
        claims_bytes = _unb64(claims_seg)
        presented_sig = _unb64(sig_seg)
    except (binascii.Error, ValueError):
        return None
    if not hmac.compare_digest(_sign(claims_bytes), presented_sig):
        return None

    try:
        claims = json.loads(claims_bytes)
        exp = int(claims["exp"])
        key_row_id = uuid.UUID(claims["kid"])
        scopes = tuple(str(s) for s in claims.get("scopes", ()))
        org = claims.get("org")
    except (json.JSONDecodeError, KeyError, TypeError, ValueError):
        return None
    if datetime.now(UTC).timestamp() >= exp:
        return None

    # The revocation check: a deleted or expired key invalidates every token
    # it ever minted, now rather than at token expiry.
    row = await db.get(ApiKey, key_row_id)
    if row is None:
        return None
    if row.expires_at is not None and row.expires_at <= datetime.now(UTC):
        return None

    return ApiKeyPrincipal(
        id=str(key_row_id),
        name=str(claims.get("name") or "unnamed"),
        scopes=scopes,
        org_id=uuid.UUID(str(org)) if org else None,
    )
