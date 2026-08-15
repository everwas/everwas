"""telemetry partitions, device_status_latest, bitemporal facts, snapshots

Revision ID: 0003
Revises: 0002
Create Date: 2026-08-15

"""

from typing import Sequence, Union

from alembic import op

revision: str = "0003"
down_revision: Union[str, None] = "0002"
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None

FACT_TABLES = ("fact_hardware", "fact_software", "fact_patch_state")


def upgrade() -> None:
    op.execute("CREATE EXTENSION IF NOT EXISTS btree_gist")

    # --- telemetry: daily range partitions; partitions themselves are managed
    # by the dispatcher's maintenance job (create ahead, drop past retention).
    op.execute("""
        CREATE TABLE telemetry_metrics (
            device_id uuid NOT NULL,
            ts timestamptz NOT NULL,
            cpu_pct real,
            mem_used bigint,
            mem_total bigint,
            swap_pct real,
            load1 real,
            uptime_s bigint,
            PRIMARY KEY (device_id, ts)
        ) PARTITION BY RANGE (ts)
    """)
    op.execute("""
        CREATE TABLE telemetry_disks (
            device_id uuid NOT NULL,
            ts timestamptz NOT NULL,
            mount text NOT NULL,
            used bigint,
            total bigint,
            fstype text,
            PRIMARY KEY (device_id, ts, mount)
        ) PARTITION BY RANGE (ts)
    """)

    op.execute("""
        CREATE TABLE device_status_latest (
            device_id uuid PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
            ts timestamptz NOT NULL,
            cpu_pct real,
            mem_pct real,
            worst_disk_pct real
        )
    """)

    op.execute("""
        CREATE TABLE device_snapshots (
            device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            kind varchar(32) NOT NULL,
            payload jsonb NOT NULL,
            snapshot_hash varchar(64) NOT NULL DEFAULT '',
            updated_at timestamptz NOT NULL DEFAULT now(),
            PRIMARY KEY (device_id, kind)
        )
    """)

    # --- bitemporal facts: identical shape x3.
    for table in FACT_TABLES:
        op.execute(f"""
            CREATE TABLE {table} (
                id bigserial PRIMARY KEY,
                device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
                fact_key text NOT NULL,
                payload jsonb NOT NULL,
                valid_during tstzrange NOT NULL,
                recorded_during tstzrange NOT NULL,
                source varchar(32) NOT NULL DEFAULT 'agent',
                CONSTRAINT {table}_no_overlapping_current EXCLUDE USING gist (
                    device_id WITH =,
                    fact_key WITH =,
                    valid_during WITH &&
                ) WHERE (upper_inf(recorded_during))
            )
        """)
        # hot path: current beliefs per device
        op.execute(f"""
            CREATE INDEX ix_{table}_current ON {table} (device_id, fact_key)
            WHERE upper_inf(recorded_during)
        """)
        # time machine: valid-time containment scans
        op.execute(f"""
            CREATE INDEX ix_{table}_valid ON {table}
            USING gist (device_id, valid_during)
        """)


def downgrade() -> None:
    for table in FACT_TABLES:
        op.execute(f"DROP TABLE {table}")
    op.execute("DROP TABLE device_snapshots")
    op.execute("DROP TABLE device_status_latest")
    op.execute("DROP TABLE telemetry_disks")
    op.execute("DROP TABLE telemetry_metrics")
