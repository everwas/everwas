"""API-key verification, shared by the MCP server and the sync API.

The key format is `ewpk_<key_id>_<secret>`; only sha256(secret) is stored.
This lived in everwas.mcp.context while MCP was the only surface that took
keys. The sync API authenticates with the same keys (exchanged for a
short-lived token, see everwas.security.sync_tokens), so verification moved
here where both can reach it without either importing the other's world.
"""

import hashlib
import hmac
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.db.engine import session_scope
from everwas.models.api_key import ApiKey

KEY_PREFIX = "ewpk_"


@dataclass(frozen=True)
class ApiKeyPrincipal:
    """Who is calling. Never carries the secret."""

    id: str
    name: str
    scopes: tuple[str, ...]
    org_id: uuid.UUID | None = None

    def has(self, scope: str) -> bool:
        return scope in self.scopes


def hash_secret(secret: str) -> str:
    return hashlib.sha256(secret.encode()).hexdigest()


def parse_api_key(raw: str) -> tuple[str, str] | None:
    """Split `ewpk_<key_id>_<secret>`. Returns None if the shape is wrong."""
    if not raw or not raw.startswith(KEY_PREFIX):
        return None
    body = raw[len(KEY_PREFIX) :]
    key_id, sep, secret = body.partition("_")
    if not sep or not key_id or not secret:
        return None
    return key_id, secret


async def authenticate_key(db: AsyncSession, raw_key: str) -> ApiKeyPrincipal | None:
    """Resolve a raw key to a principal on an existing session, or None.

    Failures are deliberately indistinguishable to the caller: unknown key id,
    wrong secret, and expired key all return None.
    """
    parsed = parse_api_key(raw_key)
    if parsed is None:
        return None
    key_id, secret = parsed

    row = (await db.execute(select(ApiKey).where(ApiKey.key_id == key_id))).scalar_one_or_none()
    if row is None:
        return None
    if not hmac.compare_digest(row.secret_hash, hash_secret(secret)):
        return None
    now = datetime.now(UTC)
    if row.expires_at is not None and row.expires_at <= now:
        return None
    row.last_used_at = now
    return ApiKeyPrincipal(
        id=str(row.id), name=row.name, scopes=tuple(row.scopes or ()), org_id=row.org_id
    )


async def authenticate(raw_key: str) -> ApiKeyPrincipal | None:
    """authenticate_key in its own session, for callers with no request scope
    (the MCP process verifies bearer tokens outside any FastAPI request)."""
    async with session_scope() as db:
        return await authenticate_key(db, raw_key)
