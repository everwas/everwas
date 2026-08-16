"""The tenant column, added before multi-tenancy exists.

These pin what it currently IS, so nobody mistakes it for an isolation
boundary, and what it must keep being, so the eventual retrofit is a query
change rather than another migration across every table.
"""

import pytest
from sqlalchemy import inspect, select, text

from openrmm.db.engine import get_engine, get_sessionmaker
from openrmm.models.device import Device, OsFamily
from openrmm.models.org import DEFAULT_ORG_ID, Organization
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

#: Every table that owns things directly. The rest reach an organization
#: through their device or script.
SCOPED = [
    "sites",
    "devices",
    "enrollment_tokens",
    "users",
    "api_keys",
    "scripts",
    "alert_rules",
    "notification_channels",
    "patch_policies",
]


async def test_every_root_scoped_table_has_the_column():
    """The whole point of doing this early. A table that misses out now needs
    its own migration later, which is exactly what this avoids."""

    def _columns(sync_conn):
        insp = inspect(sync_conn)
        return {t: {c["name"] for c in insp.get_columns(t)} for t in SCOPED}

    async with get_engine().connect() as conn:
        cols = await conn.run_sync(_columns)

    missing = [t for t, c in cols.items() if "org_id" not in c]
    assert missing == [], f"tenant column missing from {missing}"


async def test_the_default_organization_exists():
    async with get_sessionmaker()() as db:
        org = await db.get(Organization, DEFAULT_ORG_ID)
    assert org is not None
    assert org.name == "Default"


async def test_the_column_is_nullable():
    """A new row must not be forced to name an organization while nothing
    assigns one; making it required now would break every insert path for a
    feature that does not exist."""
    async with get_sessionmaker()() as db, db.begin():
        db.add(Device(id=uuid7(), hostname="no-org", os_family=OsFamily.linux))

    async with get_sessionmaker()() as db:
        rows = (await db.execute(select(Device).where(Device.hostname == "no-org"))).scalars().all()
    assert len(rows) == 1
    assert rows[0].org_id is None


async def test_an_organization_that_owns_something_cannot_be_deleted():
    """RESTRICT, not CASCADE. Deleting an organization should be refused while
    it still owns machines, not quietly take the fleet with it."""
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            Device(id=uuid7(), hostname="owned", os_family=OsFamily.linux, org_id=DEFAULT_ORG_ID)
        )

    with pytest.raises(Exception) as exc:
        async with get_sessionmaker()() as db, db.begin():
            await db.execute(
                text("DELETE FROM organizations WHERE id = :id"), {"id": str(DEFAULT_ORG_ID)}
            )
    assert "foreign key" in str(exc.value).lower() or "violates" in str(exc.value).lower()


async def test_nothing_filters_on_it_yet():
    """Documents the current truth rather than an aspiration.

    If this test ever starts failing, tenant filtering has been implemented
    and the warnings in openrmm.models.org should come out.
    """
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            Device(id=uuid7(), hostname="org-a", os_family=OsFamily.linux, org_id=DEFAULT_ORG_ID)
        )
        db.add(Device(id=uuid7(), hostname="org-none", os_family=OsFamily.linux))

    async with get_sessionmaker()() as db:
        everything = (await db.execute(select(Device))).scalars().all()
    assert len({d.hostname for d in everything}) == 2, (
        "the default device query returned fewer rows than exist, which means "
        "filtering was added; update openrmm.models.org and delete this test"
    )
