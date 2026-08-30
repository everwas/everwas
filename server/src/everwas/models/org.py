"""Organizations: the tenant boundary, present but not yet enforced.

This exists now so that turning on multi-tenancy later is a change to QUERIES
rather than a migration across every table in the schema. The column is
nullable and every existing row is backfilled to a single "Default"
organization, so nothing behaves differently today.

**Nothing filters on org_id yet.** An operator in one organization can still
see every device in the database. Do not treat this as an isolation boundary
until the queries enforce it; the column being there is not the same as it
being honoured.

Only the ROOT-scoped tables carry it. Alerts, runs, patch jobs, facts,
telemetry and shell sessions all reach an organization through their device or
script, so giving them their own copy would be denormalization to make future
queries cheaper, and that is a decision to make with real query plans rather
than in advance.
"""

import uuid
from datetime import datetime

from sqlalchemy import DateTime, String, Text, func
from sqlalchemy.orm import Mapped, mapped_column

from everwas.db.base import Base

#: The organization every pre-existing row was backfilled to. Fixed rather
#: than generated so the migration, the seed data and the tests all name the
#: same one.
DEFAULT_ORG_ID = uuid.UUID("00000000-0000-0000-0000-000000000001")


class Organization(Base):
    __tablename__ = "organizations"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(120), unique=True)
    description: Mapped[str | None] = mapped_column(Text)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())

    #: What machines in this organization should do about 802.1X: "auto",
    #: "always", "never", or None.
    #:
    #: None is a meaningful state rather than a gap: nobody has decided, so the
    #: agent's own default applies. Storing "auto" instead would look identical
    #: and would erase the difference between choosing the cautious behaviour
    #: and never having thought about it.
    network_identity: Mapped[str | None] = mapped_column(String(16), default=None)
