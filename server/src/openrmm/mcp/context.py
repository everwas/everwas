"""Session, authorization, and audit plumbing for the MCP server.

The MCP process has no request-scoped database session, so every helper here
opens its own `session_scope()`.

Authentication is an OpenRMM API key (`orpk_<key_id>_<secret>`) presented as a
bearer token. `openrmm.mcp.server.ApiKeyVerifier` turns that token into a
FastMCP `AccessToken` once per request; tools read the resulting principal back
out of the request context with `current_principal()`. No principal in context
means the call is refused, so an unauthenticated path cannot silently succeed.
"""

import hashlib
import hmac
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from datetime import UTC, datetime

import structlog
from fastmcp.exceptions import ToolError
from fastmcp.server.auth.auth import AccessToken
from fastmcp.server.dependencies import get_access_token, get_context
from sqlalchemy import select

from openrmm.db.engine import session_scope
from openrmm.models.api_key import ApiKey
from openrmm.models.audit import ActorType, AuditLog

log = structlog.get_logger()

KEY_PREFIX = "orpk_"

# Marker claims: proves an AccessToken came from our verifier and not from some
# other auth provider that happened to be mounted.
CLAIM_KEY_ID = "openrmm_key_id"
CLAIM_KEY_NAME = "openrmm_key_name"
#: The key's organization, carried on the token so that audit rows written for
#: a tool call are filed under the tenant whose key it is. Without it every MCP
#: call produced an entry with no organization, which no reader can see.
CLAIM_KEY_ORG = "openrmm_key_org"

SCOPES = (
    "devices:read",
    "alerts:read",
    "alerts:write",
    "scripts:run",
    "patches:read",
    "patches:write",
)


@dataclass(frozen=True)
class ApiKeyPrincipal:
    """Who is calling. Never carries the secret."""

    id: str
    name: str
    scopes: tuple[str, ...]
    org_id: uuid.UUID | None = None

    def has(self, scope: str) -> bool:
        return scope in self.scopes


# --- key handling ---


def hash_secret(secret: str) -> str:
    return hashlib.sha256(secret.encode()).hexdigest()


def parse_api_key(raw: str) -> tuple[str, str] | None:
    """Split `orpk_<key_id>_<secret>`. Returns None if the shape is wrong."""
    if not raw or not raw.startswith(KEY_PREFIX):
        return None
    body = raw[len(KEY_PREFIX) :]
    key_id, sep, secret = body.partition("_")
    if not sep or not key_id or not secret:
        return None
    return key_id, secret


async def authenticate(raw_key: str) -> ApiKeyPrincipal | None:
    """Resolve a bearer token to a principal, or None for any failure.

    Failures are deliberately indistinguishable to the caller: unknown key id,
    wrong secret, and expired key all return None.
    """
    parsed = parse_api_key(raw_key)
    if parsed is None:
        return None
    key_id, secret = parsed

    async with session_scope() as db:
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


def access_token_for(principal: ApiKeyPrincipal, raw_key: str) -> AccessToken:
    """Package a verified principal as the token FastMCP carries per request."""
    return AccessToken(
        token=raw_key,
        client_id=principal.id,
        scopes=list(principal.scopes),
        claims={
            CLAIM_KEY_ID: principal.id,
            CLAIM_KEY_NAME: principal.name,
            CLAIM_KEY_ORG: str(principal.org_id) if principal.org_id else None,
        },
    )


def current_principal() -> ApiKeyPrincipal:
    """The authenticated caller, or a refusal. Fails closed."""
    token = get_access_token()
    if token is None:
        raise ToolError(
            "Unauthenticated. Present an OpenRMM API key as a bearer token "
            "(Authorization: Bearer orpk_...)."
        )
    claims = token.claims or {}
    key_id = claims.get(CLAIM_KEY_ID)
    if not key_id:
        raise ToolError("This token was not issued by OpenRMM. Use an OpenRMM API key.")
    org = claims.get(CLAIM_KEY_ORG)
    return ApiKeyPrincipal(
        id=str(key_id),
        name=str(claims.get(CLAIM_KEY_NAME) or "unnamed"),
        scopes=tuple(token.scopes or ()),
        org_id=uuid.UUID(str(org)) if org else None,
    )


def require_scope(principal: ApiKeyPrincipal, scope: str) -> None:
    if not principal.has(scope):
        held = ", ".join(principal.scopes) or "none"
        raise ToolError(
            f"This API key lacks the '{scope}' scope, so the call was refused. "
            f"Scopes on this key: {held}. Ask an OpenRMM administrator for a key "
            f"with '{scope}' if this action is intended."
        )


# --- audit ---


async def audit(
    action: str,
    *,
    principal: ApiKeyPrincipal | None,
    target_type: str | None = None,
    target_id: str | None = None,
    detail: dict | None = None,
) -> None:
    """Append one audit row. Called for every tool call, success or refusal."""
    async with session_scope() as db:
        db.add(
            AuditLog(
                org_id=(principal.org_id if principal else None),
                actor_type=ActorType.api_key,
                actor_id=(principal.name[:64] if principal else None),
                action=action[:120],
                target_type=(target_type[:64] if target_type else None),
                target_id=(str(target_id)[:64] if target_id else None),
                detail=detail,
            )
        )


@dataclass
class ToolCall:
    """Mutable per-call record. Whatever lands in `detail` lands in the audit row."""

    action: str
    principal: ApiKeyPrincipal | None = None
    target_type: str | None = None
    target_id: str | None = None
    detail: dict = field(default_factory=dict)

    @property
    def actor(self) -> str:
        """`mcp:<key name>`, for requested_by / decided_by / acked_by fields."""
        if self.principal is None:
            raise ToolError("No authenticated principal on this call.")
        return f"mcp:{self.principal.name}"[:255]


@asynccontextmanager
async def tool_call(
    action: str,
    scope: str,
    *,
    target_type: str | None = None,
    target_id: str | None = None,
) -> AsyncIterator[ToolCall]:
    """Authenticate, authorize, and audit one tool call.

    The audit row is written on the way out whether the body succeeded, was
    refused for want of a scope, or blew up. A tool that never enters this
    context manager is a tool that never runs.
    """
    call = ToolCall(action=action, target_type=target_type, target_id=target_id)
    try:
        call.principal = current_principal()
        require_scope(call.principal, scope)
        yield call
    except Exception as exc:
        await audit(
            action,
            principal=call.principal,
            target_type=call.target_type,
            target_id=call.target_id,
            detail={**call.detail, "ok": False, "error": str(exc)[:500]},
        )
        raise
    await audit(
        action,
        principal=call.principal,
        target_type=call.target_type,
        target_id=call.target_id,
        detail={**call.detail, "ok": True},
    )


# --- message bus ---


def get_nats_connection():
    """The MCP process's own NATS connection, opened by the server lifespan."""
    try:
        ctx = get_context()
    except RuntimeError:
        ctx = None
    state = ctx.lifespan_context if ctx is not None else None
    nc = state.get("nats") if isinstance(state, dict) else None
    if nc is None:
        raise ToolError(
            "The RMM message bus is not connected, so nothing can be queued to agents "
            "right now. Read-only tools still work. Report this to an administrator."
        )
    return nc


# --- argument parsing, with errors an assistant can act on ---


def parse_uuid(value: str, field_name: str = "device_id") -> uuid.UUID:
    try:
        return uuid.UUID(str(value))
    except (ValueError, AttributeError, TypeError) as exc:
        raise ToolError(
            f"{field_name} must be a UUID like '018f3b2c-6a41-7c9e-9b2f-1d5a6c0e77aa'. "
            f"Got: {value!r}. Use list_devices to look one up by hostname."
        ) from exc


def parse_choice(value: str, allowed: tuple[str, ...], field_name: str) -> str:
    got = str(value).strip().lower()
    if got not in allowed:
        raise ToolError(f"{field_name} must be one of: {', '.join(allowed)}. Got: {value!r}.")
    return got


def parse_ts(value: str | None, field_name: str) -> datetime | None:
    """ISO-8601 in, aware UTC datetime out. Naive timestamps are refused."""
    if value is None or str(value).strip() == "":
        return None
    text = str(value).strip()
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ToolError(
            f"{field_name} must be an ISO-8601 timestamp, for example "
            f"'2026-08-01T13:00:00Z' or '2026-08-01T07:00:00-06:00'. Got: {value!r}."
        ) from exc
    if parsed.tzinfo is None:
        raise ToolError(
            f"{field_name}={value!r} has no timezone, which makes it ambiguous. "
            f"Add 'Z' for UTC (for example '{text}Z') or an explicit offset."
        )
    return parsed.astimezone(UTC)


def require_ts(value: str, field_name: str) -> datetime:
    parsed = parse_ts(value, field_name)
    if parsed is None:
        raise ToolError(f"{field_name} is required, for example '2026-08-01T13:00:00Z'.")
    return parsed


def iso(value: datetime | None) -> str | None:
    if value is None:
        return None
    if value.tzinfo is None:
        value = value.replace(tzinfo=UTC)
    return value.astimezone(UTC).isoformat().replace("+00:00", "Z")


def like_escape(value: str) -> str:
    """Neutralize LIKE wildcards so a substring filter stays a substring filter."""
    return value.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
