"""Security posture, one bitemporal fact per check.

Per check rather than per machine, and that is the decision this table is built
around. The set of checks GROWS: a machine assessed last month was assessed
against last month's checks, and a check added since is not a check that
machine failed, it is one that never ran on it. Per-check facts express that
directly, because a check simply has no history before it existed. A
whole-machine rollup would have to invent a belief about every check that had
never run there, and would restate the entire verdict every time any single
check moved.

It also keeps the amend granularity right. Firewall flapping amends the
firewall fact's belief window and leaves disk encryption's history untouched.

NUMBERING: this is revision 0021 revising 0019, deliberately skipping 0020.
The sync-api branch already took the id "0020" from the same parent, and two
revisions sharing an id is the one thing alembic cannot reconcile, whereas two
heads from a common parent is ordinary and is resolved with a merge revision
when the branches meet. Skipping the number is the cheap half of that problem.

Revision ID: 0021
Revises: 0019
"""

from alembic import op

revision = "0021"
down_revision = "0019"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.execute("""
        CREATE TABLE fact_posture (
            id bigserial PRIMARY KEY,
            device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            fact_key text NOT NULL,
            payload jsonb NOT NULL,
            valid_during tstzrange NOT NULL,
            recorded_during tstzrange NOT NULL,
            source varchar(32) NOT NULL DEFAULT 'agent',
            CONSTRAINT fact_posture_no_overlapping_current EXCLUDE USING gist (
                device_id WITH =,
                fact_key WITH =,
                valid_during WITH &&
            ) WHERE (upper_inf(recorded_during))
        )
    """)
    op.execute("""
        CREATE INDEX ix_fact_posture_current ON fact_posture (device_id, fact_key)
        WHERE upper_inf(recorded_during)
    """)
    op.execute("""
        CREATE INDEX ix_fact_posture_valid ON fact_posture
        USING gist (device_id, valid_during)
    """)
    # Answering "which machines are failing a check right now" is the query
    # this table exists for, and it reads the status out of the payload across
    # the whole fleet rather than one device at a time.
    op.execute("""
        CREATE INDEX ix_fact_posture_status ON fact_posture ((payload->>'status'))
        WHERE upper_inf(recorded_during)
    """)


def downgrade() -> None:
    op.execute("DROP TABLE IF EXISTS fact_posture")
