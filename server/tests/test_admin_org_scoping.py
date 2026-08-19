"""Admin surfaces create rows in the ACTING admin's organization.

The boundary was enforced on reads before it was enforced on writes, which left
it half true. create_user, mint_api_key and create_site all took the acting
admin's org (for the audit row, correctly) and then created the object without
it, so every new user, key and site landed in the default organization
regardless of who made it.

The result is worse than an unenforced boundary, because it looks enforced. An
admin in org B creates a user, sees it in their own list (it is not there), and
the user they made can see org A's fleet.

Listing is the other half: an admin must not see another organization's users,
keys or sites, or the console leaks the shape of every tenant.
"""

import uuid

import pytest

from everwas.db.engine import session_scope
from everwas.models.org import DEFAULT_ORG_ID, Organization
from everwas.models.user import Role, User

pytestmark = pytest.mark.usefixtures("pg_database")

ORG_B = uuid.UUID("0000000b-0000-0000-0000-0000000000bb")


async def _org_b() -> uuid.UUID:
    async with session_scope() as db:
        if await db.get(Organization, ORG_B) is None:
            db.add(Organization(id=ORG_B, name="admin-scope-org-b"))
    return ORG_B


async def test_a_created_user_belongs_to_the_creating_admins_org():
    from everwas.services.admin import create_user

    org_b = await _org_b()
    async with session_scope() as db:
        created = await create_user(
            db,
            email=f"u-{uuid.uuid4().hex[:8]}@example.com",
            password="a-long-enough-password",
            role=Role.operator,
            actor="admin@b.example.com",
            actor_org=org_b,
        )
    async with session_scope() as db:
        row = await db.get(User, created.id)
    assert row.org_id == org_b, (
        "a new user landed in the default organization, so the admin who made "
        "them cannot see them and they can see somebody else's fleet"
    )


async def test_a_minted_api_key_belongs_to_the_creating_admins_org():
    from everwas.services.admin import mint_api_key

    org_b = await _org_b()
    async with session_scope() as db:
        key, _secret = await mint_api_key(
            db,
            name=f"k-{uuid.uuid4().hex[:6]}",
            scopes=["devices:read"],
            ttl_days=None,
            actor="admin@b.example.com",
            actor_org=org_b,
        )
        assert key.org_id == org_b, (
            "an API key scoped to the wrong organization is a credential that "
            "reads another tenant's fleet"
        )


async def test_a_created_site_belongs_to_the_creating_admins_org():
    from everwas.services.admin import create_site

    org_b = await _org_b()
    async with session_scope() as db:
        site = await create_site(
            db, name=f"site-{uuid.uuid4().hex[:6]}", actor="admin@b.example.com", actor_org=org_b
        )
        assert site.org_id == org_b


async def test_the_default_org_is_not_a_dumping_ground():
    """Explicitly: nothing should silently choose DEFAULT_ORG_ID for a caller
    who has a real organization. The default exists to backfill history, not to
    catch new rows."""
    from everwas.services.admin import create_site

    org_b = await _org_b()
    async with session_scope() as db:
        site = await create_site(
            db, name=f"s-{uuid.uuid4().hex[:6]}", actor="a@b.example.com", actor_org=org_b
        )
    assert site.org_id != DEFAULT_ORG_ID
