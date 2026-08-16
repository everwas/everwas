"""outbox_status gains 'blocked'

Revision ID: 0009
Revises: 0008
Create Date: 2026-08-16

A channel that is missing, disabled, or misconfigured used to mark its queued
notifications `failed`, which is terminal. Disabling a channel for ten minutes
of maintenance therefore DESTROYED every critical notification queued in that
window, and a malformed payload discarded alerts fleet-wide.

`blocked` is the waiting room: the row keeps its place, retries on a slow
cadence, burns no attempts, and delivers as soon as an operator repairs the
configuration. `failed` now means only what it says: the destination itself
refused, or every retry was spent.

Existing rows are left alone. A previously-failed row cannot be distinguished
from a genuinely refused one, and silently re-queueing hours-old pages would be
its own surprise.
"""

from collections.abc import Sequence

from alembic import op

revision: str = "0009"
down_revision: str | None = "0008"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    # IF NOT EXISTS so a re-run on a partially migrated database is harmless.
    # Nothing below may USE the new label: PostgreSQL allows ADD VALUE inside a
    # transaction but refuses to let the same transaction reference it, which
    # is why there is no partial index on ('pending', 'blocked') here.
    op.execute("ALTER TYPE outbox_status ADD VALUE IF NOT EXISTS 'blocked' AFTER 'sent'")


def downgrade() -> None:
    # PostgreSQL cannot drop a value from an enum type. Rows in 'blocked' are
    # moved back to 'pending' so nothing is stranded on a value the older code
    # does not select; the unused label stays in the type.
    op.execute("UPDATE notification_outbox SET status = 'pending' WHERE status = 'blocked'")
