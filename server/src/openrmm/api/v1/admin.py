"""Admin surfaces: users, API keys, sites.

Admin-only throughout. These change who can reach the fleet, so every route
here is a `require_role(Role.admin)` and every mutation is audited.
"""

import uuid

import structlog
from fastapi import APIRouter, HTTPException, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession, require_role
from openrmm.models.api_key import ApiKey
from openrmm.models.device import Site
from openrmm.models.user import Role, User
from openrmm.schemas.admin import (
    ApiKeyCreated,
    ApiKeyIn,
    ApiKeyOut,
    SiteIn,
    SiteOut,
    UserIn,
    UserOut,
    UserRoleIn,
)
from openrmm.security.tenancy import caller_org, scope_to_org
from openrmm.services.admin import (
    KNOWN_SCOPES,
    AdminError,
    create_site,
    create_user,
    delete_site,
    mint_api_key,
    rename_site,
    revoke_api_key,
    set_user_active,
    set_user_role,
)

router = APIRouter(dependencies=[require_role(Role.admin)])
log = structlog.get_logger()


def _refused(exc: AdminError) -> HTTPException:
    # 409, not 400: these are all "the current state does not allow this",
    # which is a different thing from a malformed request and reads
    # differently to whoever is looking at it.
    return HTTPException(status.HTTP_409_CONFLICT, str(exc))


# ---------------------------------------------------------------- users


@router.get("/users")
async def list_users(db: DbSession, _user: CurrentUser) -> list[UserOut]:
    rows = await db.execute(
        scope_to_org(select(User).order_by(User.email), User.org_id, caller_org(_user))
    )
    return [UserOut.model_validate(u) for u in rows.scalars()]


@router.post("/users", status_code=status.HTTP_201_CREATED)
async def add_user(body: UserIn, db: DbSession, user: CurrentUser) -> UserOut:
    try:
        created = await create_user(
            db,
            email=body.email,
            password=body.password,
            role=body.role,
            actor=user.email,
            actor_org=user.org_id,
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return UserOut.model_validate(created)


@router.post("/users/{user_id}/role")
async def change_role(
    user_id: uuid.UUID, body: UserRoleIn, db: DbSession, user: CurrentUser
) -> UserOut:
    try:
        updated = await set_user_role(
            db, user_id, role=body.role, actor=user.email, actor_org=user.org_id
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return UserOut.model_validate(updated)


@router.post("/users/{user_id}/disable")
async def disable_user(user_id: uuid.UUID, db: DbSession, user: CurrentUser) -> UserOut:
    try:
        updated = await set_user_active(
            db, user_id, active=False, actor=user.email, actor_org=user.org_id
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return UserOut.model_validate(updated)


@router.post("/users/{user_id}/enable")
async def enable_user(user_id: uuid.UUID, db: DbSession, user: CurrentUser) -> UserOut:
    try:
        updated = await set_user_active(
            db, user_id, active=True, actor=user.email, actor_org=user.org_id
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return UserOut.model_validate(updated)


# ------------------------------------------------------------- api keys


@router.get("/api-keys")
async def list_api_keys(db: DbSession, _user: CurrentUser) -> list[ApiKeyOut]:
    rows = await db.execute(
        scope_to_org(
            select(ApiKey).order_by(ApiKey.created_at.desc()), ApiKey.org_id, caller_org(_user)
        )
    )
    return [ApiKeyOut.model_validate(k) for k in rows.scalars()]


@router.get("/api-keys/scopes")
async def list_scopes(_user: CurrentUser) -> list[str]:
    """So the UI offers the real set instead of a free-text box where a typo
    mints a key that looks privileged and can do nothing."""
    return sorted(KNOWN_SCOPES)


@router.post("/api-keys", status_code=status.HTTP_201_CREATED)
async def add_api_key(body: ApiKeyIn, db: DbSession, user: CurrentUser) -> ApiKeyCreated:
    """The response carries the key in plaintext. It is the only time it
    exists in readable form; only sha256(secret) is stored."""
    try:
        key, plaintext = await mint_api_key(
            db,
            name=body.name,
            scopes=body.scopes,
            ttl_days=body.ttl_days,
            actor=user.email,
            actor_org=user.org_id,
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return ApiKeyCreated(key=ApiKeyOut.model_validate(key), secret=plaintext)


@router.delete("/api-keys/{key_id}", status_code=status.HTTP_204_NO_CONTENT)
async def delete_api_key(key_id: uuid.UUID, db: DbSession, user: CurrentUser) -> None:
    try:
        await revoke_api_key(db, key_id, actor=user.email, actor_org=user.org_id)
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()


# ---------------------------------------------------------------- sites


@router.get("/sites")
async def list_sites(db: DbSession, _user: CurrentUser) -> list[SiteOut]:
    rows = await db.execute(
        scope_to_org(select(Site).order_by(Site.name), Site.org_id, caller_org(_user))
    )
    return [SiteOut.model_validate(s) for s in rows.scalars()]


@router.post("/sites", status_code=status.HTTP_201_CREATED)
async def add_site(body: SiteIn, db: DbSession, user: CurrentUser) -> SiteOut:
    try:
        site = await create_site(
            db,
            name=body.name,
            actor=user.email,
            actor_org=user.org_id,
            description=body.description,
            address=body.address,
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return SiteOut.model_validate(site)


@router.put("/sites/{site_id}")
async def edit_site(site_id: uuid.UUID, body: SiteIn, db: DbSession, user: CurrentUser) -> SiteOut:
    try:
        site = await rename_site(
            db,
            site_id,
            name=body.name,
            actor=user.email,
            actor_org=user.org_id,
            description=body.description,
            address=body.address,
        )
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
    return SiteOut.model_validate(site)


@router.delete("/sites/{site_id}", status_code=status.HTTP_204_NO_CONTENT)
async def remove_site(site_id: uuid.UUID, db: DbSession, user: CurrentUser) -> None:
    try:
        await delete_site(db, site_id, actor=user.email, actor_org=user.org_id)
    except AdminError as exc:
        raise _refused(exc) from exc
    await db.commit()
