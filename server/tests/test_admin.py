"""Users, API keys and sites.

All three tables existed from M0 with no API, so adding an operator or
rotating the key the MCP server authenticates with meant shell access to the
server. That is a bad shape for the thing meant to REPLACE shell access.

The interesting tests here are the refusals: this surface decides who can
reach the fleet, and every one of these is a way to lock yourself out or to
hand out something that does not work.
"""

import hashlib

import pytest
from sqlalchemy import select

from openrmm.db.engine import get_sessionmaker
from openrmm.mcp.context import parse_api_key
from openrmm.models.api_key import ApiKey
from openrmm.models.audit import AuditLog
from openrmm.models.device import Device, OsFamily, Site
from openrmm.models.user import Role, User
from openrmm.security.passwords import hash_password
from openrmm.services.admin import (
    AdminError,
    create_site,
    create_user,
    delete_site,
    mint_api_key,
    set_user_active,
    set_user_role,
)
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _admin(email="root@example.com") -> User:
    async with get_sessionmaker()() as db, db.begin():
        u = User(email=email, password_hash=hash_password("x" * 12), role=Role.admin)
        db.add(u)
    return u


async def test_the_last_admin_cannot_be_demoted():
    """Otherwise the admin-only surface has nobody who can reach it, and the
    fix is a shell on the server, which is what this exists to avoid."""
    admin = await _admin()
    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="last active admin"):
            await set_user_role(db, admin.id, role=Role.viewer, actor="root@example.com")


async def test_the_last_admin_can_be_demoted_once_there_is_another():
    admin = await _admin()
    async with get_sessionmaker()() as db, db.begin():
        await create_user(
            db,
            email="second@example.com",
            password="x" * 12,
            role=Role.admin,
            actor="root@example.com",
        )
    async with get_sessionmaker()() as db, db.begin():
        assert await set_user_role(db, admin.id, role=Role.viewer, actor="root@example.com")


async def test_you_cannot_disable_yourself():
    """The specific way an admin locks themselves out with one click."""
    admin = await _admin()
    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="your own account"):
            await set_user_active(db, admin.id, active=False, actor="root@example.com")


async def test_a_disabled_admin_does_not_count_as_cover():
    """A disabled account cannot log in, so it cannot re-enable anyone. If it
    counted, demoting the only usable admin would be allowed."""
    admin = await _admin()
    async with get_sessionmaker()() as db, db.begin():
        other = await create_user(
            db, email="b@example.com", password="x" * 12, role=Role.admin, actor="root@example.com"
        )
        other_id = other.id
    async with get_sessionmaker()() as db, db.begin():
        await set_user_active(db, other_id, active=False, actor="root@example.com")

    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="last active admin"):
            await set_user_role(db, admin.id, role=Role.viewer, actor="root@example.com")


async def test_duplicate_email_is_refused():
    async with get_sessionmaker()() as db, db.begin():
        await create_user(
            db, email="dup@example.com", password="x" * 12, role=Role.viewer, actor="a@b.c"
        )
    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="already has an account"):
            await create_user(
                db, email="dup@example.com", password="x" * 12, role=Role.viewer, actor="a@b.c"
            )


async def test_only_the_hash_is_stored():
    """The plaintext is returned once and never again. If the secret were
    stored, a database read would be every integration's credentials."""
    async with get_sessionmaker()() as db, db.begin():
        key, plaintext = await mint_api_key(
            db, name="mcp", scopes=["devices:read"], ttl_days=30, actor="a@b.c"
        )
        key_id = key.id

    # Parsed with the PRODUCTION parser, not by hand. token_urlsafe emits "_"
    # about half the time, so rsplit("_") lands inside the secret and this
    # test failed only on those runs. The real parser partitions on the FIRST
    # underscore after the prefix, which is unambiguous because key_id is hex.
    assert plaintext.startswith("orpk_")
    parsed = parse_api_key(plaintext)
    assert parsed is not None, f"the production parser rejected a key it minted: {plaintext[:16]}…"
    parsed_key_id, secret = parsed

    async with get_sessionmaker()() as db:
        stored = await db.get(ApiKey, key_id)
    assert parsed_key_id == stored.key_id
    assert secret not in stored.secret_hash
    assert stored.secret_hash == hashlib.sha256(secret.encode()).hexdigest()


async def test_an_unknown_scope_is_refused():
    """Accepting it produces a key that looks privileged and can do nothing,
    which is debugged by nobody."""
    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="unknown scope"):
            await mint_api_key(
                db, name="typo", scopes=["devices:reed"], ttl_days=None, actor="a@b.c"
            )


async def test_a_key_with_no_scopes_is_refused():
    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="at least one"):
            await mint_api_key(db, name="empty", scopes=[], ttl_days=None, actor="a@b.c")


async def test_two_keys_never_share_a_secret():
    async with get_sessionmaker()() as db, db.begin():
        _, a = await mint_api_key(
            db, name="a", scopes=["devices:read"], ttl_days=None, actor="a@b.c"
        )
        _, b = await mint_api_key(
            db, name="b", scopes=["devices:read"], ttl_days=None, actor="a@b.c"
        )
    assert a != b


async def test_a_site_with_devices_cannot_be_deleted():
    """devices.site_id is ON DELETE SET NULL, so this would SUCCEED and
    silently orphan every machine into 'no site'. The only sign would be a
    fleet list that had quietly lost its grouping."""
    async with get_sessionmaker()() as db, db.begin():
        site = await create_site(db, name="HQ", actor="a@b.c")
        await db.flush()
        db.add(Device(id=uuid7(), hostname="in-hq", os_family=OsFamily.linux, site_id=site.id))
        site_id = site.id

    async with get_sessionmaker()() as db, db.begin():
        with pytest.raises(AdminError, match="still has devices"):
            await delete_site(db, site_id, actor="a@b.c")

    async with get_sessionmaker()() as db:
        assert await db.get(Site, site_id) is not None


async def test_an_empty_site_can_be_deleted():
    async with get_sessionmaker()() as db, db.begin():
        site = await create_site(db, name="Empty", actor="a@b.c")
        site_id = site.id
    async with get_sessionmaker()() as db, db.begin():
        await delete_site(db, site_id, actor="a@b.c")
    async with get_sessionmaker()() as db:
        assert await db.get(Site, site_id) is None


async def test_every_mutation_is_audited():
    """This surface decides who can reach the fleet. An unaudited change to it
    is the one nobody can reconstruct afterwards."""
    async with get_sessionmaker()() as db, db.begin():
        await create_user(
            db, email="new@example.com", password="x" * 12, role=Role.viewer, actor="a@b.c"
        )
        await mint_api_key(db, name="k", scopes=["devices:read"], ttl_days=None, actor="a@b.c")
        await create_site(db, name="Branch", actor="a@b.c")

    async with get_sessionmaker()() as db:
        actions = set((await db.execute(select(AuditLog.action))).scalars())
    assert {"user.created", "api_key.created", "site.created"} <= actions


async def test_a_secret_containing_an_underscore_still_parses():
    """token_urlsafe emits "_" and "-", so about half of all keys contain one.
    The format is `orpk_<key_id>_<secret>` and only the FIRST underscore after
    the prefix is a delimiter, which works because key_id is hex. Splitting on
    the last one instead silently truncates the secret, and every key with an
    underscore fails to authenticate."""
    for _ in range(40):
        async with get_sessionmaker()() as db, db.begin():
            key, plaintext = await mint_api_key(
                db, name="k", scopes=["devices:read"], ttl_days=None, actor="a@b.c"
            )
            expected_id, expected_hash = key.key_id, key.secret_hash

        parsed = parse_api_key(plaintext)
        assert parsed is not None
        key_id, secret = parsed
        assert key_id == expected_id
        assert hashlib.sha256(secret.encode()).hexdigest() == expected_hash
