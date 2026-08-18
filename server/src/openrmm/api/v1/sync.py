"""/api/v1/sync — the read-only surface for external systems of record.

Built for one access pattern: sweep the whole (org-scoped) fleet in pages,
never call per device. That is the pattern SSoT consumers are written
against, and it is why serial numbers ride the device LIST here when the
SPA's own list payload carries no identity at all.

Everything authenticates with a bearer sync token (see api.sync_deps) and a
scope, never a session or a role. Every endpoint pages identically (see
api.pagination). The three fact sweeps accept the bitemporal parameters:
as_of answers "what was true on the machines at T", knew_at answers "what
did the server believe at T" — both timezone-required, both defaulting to
current belief. The contract, including which fields are volatile and
should be excluded from diffs, lives in docs/sync-api.md.
"""

import uuid
from datetime import datetime
from typing import Annotated

from fastapi import APIRouter, HTTPException, Query, status

from openrmm.api.deps import DbSession
from openrmm.api.pagination import (
    DEFAULT_LIMIT,
    MAX_LIMIT,
    decode_cursor,
    encode_cursor,
    require_aware,
)
from openrmm.api.sync_deps import SyncPrincipal, require_sync_scope
from openrmm.schemas.sync import (
    SyncDevicePage,
    SyncInterfacePage,
    SyncOrgPage,
    SyncPatchPage,
    SyncSitePage,
    SyncSoftwarePage,
)
from openrmm.services import sync_export

router = APIRouter()

Limit = Annotated[int, Query(ge=1, le=MAX_LIMIT)]


def _uuid_cursor(cursor: str | None) -> uuid.UUID | None:
    if cursor is None:
        return None
    (raw,) = decode_cursor(cursor, parts=1)
    try:
        return uuid.UUID(raw)
    except ValueError as exc:
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_ENTITY, "cursor does not fit this endpoint"
        ) from exc


def _fact_cursor(cursor: str | None) -> tuple[uuid.UUID, str] | None:
    if cursor is None:
        return None
    device_raw, fact_key = decode_cursor(cursor, parts=2)
    try:
        return uuid.UUID(device_raw), fact_key
    except ValueError as exc:
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_ENTITY, "cursor does not fit this endpoint"
        ) from exc


@router.get("/organizations", dependencies=[require_sync_scope("devices:read")])
async def organizations(db: DbSession, principal: SyncPrincipal) -> SyncOrgPage:
    """The caller's organization — one element, page-shaped like everything
    else so a consumer walks all sync endpoints with the same loop."""
    items = await sync_export.org_page(db, principal)
    return SyncOrgPage(items=items, has_more=False, next_cursor=None)


@router.get("/sites", dependencies=[require_sync_scope("devices:read")])
async def sites(
    db: DbSession,
    principal: SyncPrincipal,
    cursor: str | None = None,
    limit: Limit = DEFAULT_LIMIT,
) -> SyncSitePage:
    items, has_more = await sync_export.site_page(
        db, principal, cursor_id=_uuid_cursor(cursor), limit=limit
    )
    return SyncSitePage(
        items=items,
        has_more=has_more,
        next_cursor=encode_cursor([str(items[-1].id)]) if has_more else None,
    )


@router.get("/devices", dependencies=[require_sync_scope("devices:read")])
async def devices(
    db: DbSession,
    principal: SyncPrincipal,
    site_id: uuid.UUID | None = None,
    cursor: str | None = None,
    limit: Limit = DEFAULT_LIMIT,
) -> SyncDevicePage:
    """The devices-detailed sweep: identity, placement, hardware, and
    address rollup in the list payload, so a consumer never needs a
    per-device follow-up. Retired devices are included — status says so,
    and whether "retired" means "remove from the SSoT" is the consumer's
    policy, not this API's guess."""
    items, has_more = await sync_export.device_page(
        db, principal, site_id=site_id, cursor_id=_uuid_cursor(cursor), limit=limit
    )
    return SyncDevicePage(
        items=items,
        has_more=has_more,
        next_cursor=encode_cursor([str(items[-1].id)]) if has_more else None,
    )


def _sweep_params(
    device_id: uuid.UUID | None,
    site_id: uuid.UUID | None,
    cursor: str | None,
    limit: int,
    as_of: datetime | None,
    knew_at: datetime | None,
) -> dict:
    return {
        "device_id": device_id,
        "site_id": site_id,
        "cursor": _fact_cursor(cursor),
        "limit": limit,
        "as_of": require_aware("as_of", as_of),
        "knew_at": require_aware("knew_at", knew_at),
    }


def _fact_next_cursor(last: tuple | None) -> str | None:
    return encode_cursor([str(last[0]), last[1]]) if last else None


@router.get("/interfaces", dependencies=[require_sync_scope("devices:read")])
async def interfaces(
    db: DbSession,
    principal: SyncPrincipal,
    device_id: uuid.UUID | None = None,
    site_id: uuid.UUID | None = None,
    cursor: str | None = None,
    limit: Limit = DEFAULT_LIMIT,
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> SyncInterfacePage:
    items, has_more, last = await sync_export.interface_page(
        db, principal, **_sweep_params(device_id, site_id, cursor, limit, as_of, knew_at)
    )
    return SyncInterfacePage(items=items, has_more=has_more, next_cursor=_fact_next_cursor(last))


@router.get("/software", dependencies=[require_sync_scope("devices:read")])
async def software(
    db: DbSession,
    principal: SyncPrincipal,
    device_id: uuid.UUID | None = None,
    site_id: uuid.UUID | None = None,
    cursor: str | None = None,
    limit: Limit = DEFAULT_LIMIT,
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> SyncSoftwarePage:
    items, has_more, last = await sync_export.software_page(
        db, principal, **_sweep_params(device_id, site_id, cursor, limit, as_of, knew_at)
    )
    return SyncSoftwarePage(items=items, has_more=has_more, next_cursor=_fact_next_cursor(last))


@router.get("/patches", dependencies=[require_sync_scope("patches:read")])
async def patches(
    db: DbSession,
    principal: SyncPrincipal,
    device_id: uuid.UUID | None = None,
    site_id: uuid.UUID | None = None,
    cursor: str | None = None,
    limit: Limit = DEFAULT_LIMIT,
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> SyncPatchPage:
    items, has_more, last = await sync_export.patch_page(
        db, principal, **_sweep_params(device_id, site_id, cursor, limit, as_of, knew_at)
    )
    return SyncPatchPage(items=items, has_more=has_more, next_cursor=_fact_next_cursor(last))
