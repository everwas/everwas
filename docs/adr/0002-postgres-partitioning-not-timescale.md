# ADR-0002: Plain PostgreSQL partitioning for telemetry, not TimescaleDB

Status: accepted (2026-08-15)

## Context

Telemetry is append-only time-series (device metrics every 60 s). TimescaleDB
is the obvious tool; declarative range partitioning is the boring alternative.

## Decision

Daily range-partitioned plain PostgreSQL tables, partitions created and dropped
by a dispatcher maintenance job. A non-partitioned `device_status_latest` hot
cache serves the dashboard.

## Rationale

- TimescaleDB's compelling features (compression, continuous aggregates) sit
  under the Timescale License, which is awkward inside an AGPL distribution and
  rules out the stock `postgres` image self-hosters expect.
- v1 volumes (1k devices ≈ 1.4M rows/day) are trivial for partitioned PG.
- Retention is `DROP TABLE` on old partitions — no vacuum churn.

## Consequences

- We own partition maintenance (a dispatcher scheduled job + tests).
- If fleets reach a scale where compression matters, revisit — the ingest path
  is isolated in `ingest/telemetry.py`, so a storage swap stays contained.

Bitemporal inventory facts are unrelated to this choice: they are regular
tables with `tstzrange` columns (see `docs/bitemporal.md`, written at M2).
