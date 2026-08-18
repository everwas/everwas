"""Users, API keys and sites: the tables that could only be reached by CLI.

All three existed from M0 and none had an API, so adding an operator or
rotating the key the MCP server authenticates with meant shell access to the
server. That is a bad shape for the thing that is supposed to REPLACE shell
access to servers.
"""

import hashlib
import secrets
import uuid
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.api_key import ApiKey
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, Site
from openrmm.models.user import Role, User
from openrmm.security.passwords import hash_password

log = structlog.get_logger()

#: The acting operator's organization, which every entry in this module is
#: filed under. These functions take an email rather than a User, so the org
#: has to travel beside it; the alternative was deriving it from the row being
#: created, and a user or key created by an admin in one tenant would then file
#: its own creation under whichever tenant it landed in.
OrgId = uuid.UUID | None

#: Scopes an API key may hold. Anything not in here is a typo, and a typo in a
#: scope grants nothing while looking like it granted something.
KNOWN_SCOPES = frozenset(
    {
        "devices:read",
        "devices:write",
        "alerts:read",
        "alerts:write",
        "patches:read",
        "patches:write",
        "scripts:read",
        "scripts:run",
        "audit:read",
    }
)


class AdminError(Exception):
    """A refusal the caller should show to the operator verbatim."""


# ---------------------------------------------------------------- users


async def create_user(
    db: AsyncSession, *, email: str, password: str, role: Role, actor: str, actor_org: OrgId
) -> User:
    existing = (await db.execute(select(User).where(User.email == email))).scalar_one_or_none()
    if existing is not None:
        raise AdminError(f"{email} already has an account")

    user = User(org_id=actor_org, email=email, password_hash=hash_password(password), role=role)
    db.add(user)
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="user.created",
            target_type="user",
            target_id=email,
            detail={"role": role.value},
        )
    )
    await db.flush()
    return user


async def set_user_active(
    db: AsyncSession, user_id: uuid.UUID, *, active: bool, actor: str, actor_org: OrgId
) -> User:
    """Disable rather than delete. A deleted user's name vanishes from every
    audit entry they ever produced; a disabled one cannot log in and the trail
    still says who did what."""
    user = await db.get(User, user_id)
    if user is None:
        raise AdminError("unknown user")
    if not active and user.email == actor:
        # Locking yourself out of an admin-only surface leaves nobody able to
        # re-enable anyone.
        raise AdminError("you cannot disable your own account")
    if not active and user.role is Role.admin and await _last_active_admin(db, user_id):
        raise AdminError("this is the last active admin; promote somebody else before disabling it")

    user.is_active = active
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="user.enabled" if active else "user.disabled",
            target_type="user",
            target_id=user.email,
        )
    )
    await db.flush()
    return user


async def set_user_role(
    db: AsyncSession, user_id: uuid.UUID, *, role: Role, actor: str, actor_org: OrgId
) -> User:
    user = await db.get(User, user_id)
    if user is None:
        raise AdminError("unknown user")
    if role is not Role.admin and user.role is Role.admin and await _last_active_admin(db, user_id):
        raise AdminError("this is the last active admin; promote somebody else first")

    was, user.role = user.role, role
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="user.role_changed",
            target_type="user",
            target_id=user.email,
            detail={"from": was.value, "to": role.value},
        )
    )
    await db.flush()
    return user


async def _last_active_admin(db: AsyncSession, excluding: uuid.UUID) -> bool:
    rows = await db.execute(
        select(User.id).where(User.role == Role.admin, User.is_active, User.id != excluding)
    )
    return rows.first() is None


# ------------------------------------------------------------- api keys


async def mint_api_key(
    db: AsyncSession,
    *,
    name: str,
    scopes: list[str],
    ttl_days: int | None,
    actor: str,
    actor_org: OrgId,
) -> tuple[ApiKey, str]:
    """Create a key and return it in plaintext ONCE.

    Only sha256(secret) is stored, so this return value is the only time the
    key exists in readable form. The caller has to show it and mean it.
    """
    if unknown := sorted(set(scopes) - KNOWN_SCOPES):
        # Silently accepting an unknown scope produces a key that looks
        # privileged and can do nothing, which is debugged by nobody.
        raise AdminError(f"unknown scope(s): {', '.join(unknown)}")
    if not scopes:
        raise AdminError("a key with no scopes can do nothing; give it at least one")

    key_id = secrets.token_hex(11)
    secret = secrets.token_urlsafe(32)
    key = ApiKey(
        org_id=actor_org,
        name=name,
        key_id=key_id,
        secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
        scopes=scopes,
        expires_at=datetime.now(UTC) + timedelta(days=ttl_days) if ttl_days else None,
    )
    db.add(key)
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="api_key.created",
            target_type="api_key",
            target_id=key_id,
            detail={"name": name, "scopes": scopes, "ttl_days": ttl_days},
        )
    )
    await db.flush()
    log.info("api key minted", key_id=key_id, name=name, actor=actor)
    return key, f"orpk_{key_id}_{secret}"


async def revoke_api_key(
    db: AsyncSession, key_id: uuid.UUID, actor: str, *, actor_org: OrgId
) -> ApiKey:
    key = await db.get(ApiKey, key_id)
    if key is None:
        raise AdminError("unknown key")
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="api_key.revoked",
            target_type="api_key",
            target_id=key.key_id,
            detail={"name": key.name},
        )
    )
    await db.delete(key)
    await db.flush()
    return key


# ---------------------------------------------------------------- sites


async def create_site(db: AsyncSession, *, name: str, actor: str, actor_org: OrgId) -> Site:
    existing = (await db.execute(select(Site).where(Site.name == name))).scalar_one_or_none()
    if existing is not None:
        raise AdminError(f"a site called {name!r} already exists")
    # The acting admin's organization, not the default. The boundary was
    # enforced on reads before writes, which left it half true: a new site
    # landed in the default org, so the admin who made it could not see it and
    # anyone in the default org could.
    site = Site(name=name, org_id=actor_org)
    db.add(site)
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="site.created",
            target_type="site",
            target_id=name,
        )
    )
    await db.flush()
    return site


async def rename_site(
    db: AsyncSession, site_id: uuid.UUID, *, name: str, actor: str, actor_org: OrgId
) -> Site:
    """Rename in place. Devices reference the site by id, so nothing moves."""
    site = await db.get(Site, site_id)
    if site is None:
        raise AdminError("unknown site")
    clash = (
        await db.execute(select(Site).where(Site.name == name, Site.id != site_id))
    ).scalar_one_or_none()
    if clash is not None:
        raise AdminError(f"a site called {name!r} already exists")

    was, site.name = site.name, name
    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="site.renamed",
            target_type="site",
            target_id=str(site_id),
            detail={"from": was, "to": name},
        )
    )
    await db.flush()
    return site


async def delete_site(
    db: AsyncSession, site_id: uuid.UUID, actor: str, *, actor_org: OrgId
) -> Site:
    """Refused while devices still point at it.

    devices.site_id is ON DELETE SET NULL, so deleting a populated site would
    silently orphan its machines into "no site" rather than failing, and the
    only sign would be a fleet list that had quietly lost its grouping.
    """
    site = await db.get(Site, site_id)
    if site is None:
        raise AdminError("unknown site")
    rows = await db.execute(select(Device.id).where(Device.site_id == site_id).limit(1))
    if rows.first() is not None:
        raise AdminError("this site still has devices; move them first")

    db.add(
        AuditLog(
            org_id=actor_org,
            actor_type=ActorType.user,
            actor_id=actor,
            action="site.deleted",
            target_type="site",
            target_id=site.name,
        )
    )
    await db.delete(site)
    await db.flush()
    return site
