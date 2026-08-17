"""Interactive login sessions.

Bitemporal rather than latest-only for the same reason the interfaces are: the
useful version of "who is logged in" is usually asked in the past tense, after
something changed and somebody wants to know who was on the box at the time. A
current-state table answers the first question and destroys the second.

One fact per user-and-seat rather than per login event, which keeps the key set
bounded by the number of distinct places a person can sit rather than growing
without limit. A second login on the same seat is a genuine amend, and the
sequenced-amend history still records the succession.

Revision ID: 0014
Revises: 0013
"""

from alembic import op

revision = "0014"
down_revision = "0013"
branch_labels = None
depends_on = None


def upgrade() -> None:
    # Same shape as the other fact tables (migrations 0003 and 0013).
    op.execute("""
        CREATE TABLE fact_logins (
            id bigserial PRIMARY KEY,
            device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            fact_key text NOT NULL,
            payload jsonb NOT NULL,
            valid_during tstzrange NOT NULL,
            recorded_during tstzrange NOT NULL,
            source varchar(32) NOT NULL DEFAULT 'agent',
            CONSTRAINT fact_logins_no_overlapping_current EXCLUDE USING gist (
                device_id WITH =,
                fact_key WITH =,
                valid_during WITH &&
            ) WHERE (upper_inf(recorded_during))
        )
    """)
    op.execute("""
        CREATE INDEX ix_fact_logins_current ON fact_logins (device_id, fact_key)
        WHERE upper_inf(recorded_during)
    """)
    op.execute("""
        CREATE INDEX ix_fact_logins_valid ON fact_logins
        USING gist (device_id, valid_during)
    """)


def downgrade() -> None:
    op.execute("DROP TABLE IF EXISTS fact_logins")
