"""Track the highest applied output sequence per stream.

JetStream is at-least-once and the dispatcher acks after committing, so any
process death between the two redelivers a chunk that was already applied.
apply_job_output appended unconditionally, so the second delivery appended the
same block again: a 256 KiB duplicate mid-stream, in output an operator reads
or something downstream parses.

The agent has always framed chunks with a monotonic `seq`; nothing read it.
One high-water mark per stream is enough, because a stream's chunks are ordered
and the only question is whether this one has been seen.

Revision ID: 0015
Revises: 0014
"""

import sqlalchemy as sa
from alembic import op

revision = "0015"
down_revision = "0014"
branch_labels = None
depends_on = None

# -1 rather than 0: seq starts at 0, so 0 must be a value that has NOT been
# applied yet, and NULL would make every comparison below need a guard.
NONE_APPLIED = "-1"


def upgrade() -> None:
    for stream in ("stdout", "stderr"):
        op.add_column(
            "script_runs",
            sa.Column(
                f"{stream}_seq",
                sa.Integer(),
                nullable=False,
                server_default=sa.text(NONE_APPLIED),
            ),
        )


def downgrade() -> None:
    op.drop_column("script_runs", "stderr_seq")
    op.drop_column("script_runs", "stdout_seq")
