"""Merge the sync-metadata and posture branches back into one head.

Two revisions left 0019 in parallel: 0020 (org and site descriptions for the
sync surface) on the sync-api branch, and 0021 (the fact_posture table) on the
posture branch. 0021 skipped the id 0020 deliberately — that id was already
taken from the same parent, and two revisions sharing an id is the one thing
alembic cannot reconcile, whereas two heads from a common parent is ordinary
and is resolved exactly here, with a merge revision, when the branches meet.

Empty on purpose. The branches touch disjoint objects (three nullable text
columns vs. a new table), so nothing needs reconciling; this revision exists
only so `alembic upgrade head` has a single head to aim at again.

Revision ID: 0022
Revises: 0020, 0021
"""

revision = "0022"
down_revision = ("0020", "0021")
branch_labels = None
depends_on = None


def upgrade() -> None:
    pass


def downgrade() -> None:
    pass
