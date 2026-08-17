"""Per-interface network telemetry, plus the interface inventory it labels.

Two halves of one feature, both of which the agent had already been sending
into a void: telemetry arrived with a `nets` array that ingest dropped, and the
`network` inventory kind was rejected outright because it was in neither
FACT_KINDS nor SNAPSHOT_KINDS.

Counters are stored RAW and cumulative, exactly as the kernel reports them.
Rates are derived at query time instead of at ingest, for three reasons: the
raw counter is the only thing that is actually true, a redelivered or
out-of-order sample cannot corrupt stored state the way an incrementally
maintained delta can, and the averaging window stays a query decision rather
than being baked into the data at write time.

Interfaces go in a bitemporal fact table rather than a latest-only snapshot
because the question an incident starts with is "what address did this box have
at 03:00 last Tuesday", and a current-state table cannot answer it.

Revision ID: 0013
Revises: 0012
"""

from alembic import op

revision = "0013"
down_revision = "0012"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.execute("""
        CREATE TABLE telemetry_network (
            device_id uuid NOT NULL,
            ts timestamptz NOT NULL,
            iface text NOT NULL,
            bytes_sent bigint,
            bytes_recv bigint,
            packets_sent bigint,
            packets_recv bigint,
            err_in bigint,
            err_out bigint,
            drop_in bigint,
            drop_out bigint,
            PRIMARY KEY (device_id, ts, iface)
        ) PARTITION BY RANGE (ts)
    """)

    # The primary key leads with device_id, which serves "one device, one
    # window" fine. This index exists for the per-interface series a chart
    # actually draws: without iface in front of ts, reading one NIC's history
    # means scanning every NIC's rows for that device and discarding most.
    op.execute("""
        CREATE INDEX ix_telemetry_network_series
            ON telemetry_network (device_id, iface, ts DESC)
    """)

    # Same shape as the other three fact tables (migration 0003). The GiST
    # exclusion is the safety net under the sequenced-amend writer: it rejects
    # two current beliefs whose valid-time windows overlap for one fact_key.
    op.execute("""
        CREATE TABLE fact_network (
            id bigserial PRIMARY KEY,
            device_id uuid NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
            fact_key text NOT NULL,
            payload jsonb NOT NULL,
            valid_during tstzrange NOT NULL,
            recorded_during tstzrange NOT NULL,
            source varchar(32) NOT NULL DEFAULT 'agent',
            CONSTRAINT fact_network_no_overlapping_current EXCLUDE USING gist (
                device_id WITH =,
                fact_key WITH =,
                valid_during WITH &&
            ) WHERE (upper_inf(recorded_during))
        )
    """)
    op.execute("""
        CREATE INDEX ix_fact_network_current ON fact_network (device_id, fact_key)
        WHERE upper_inf(recorded_during)
    """)
    op.execute("""
        CREATE INDEX ix_fact_network_valid ON fact_network
        USING gist (device_id, valid_during)
    """)


def downgrade() -> None:
    op.execute("DROP TABLE IF EXISTS fact_network")
    op.execute("DROP TABLE IF EXISTS telemetry_network")
