---
title: "ADR-0002: Plain PostgreSQL partitioning for telemetry, not TimescaleDB"
description: Daily range partitions and a hot cache instead of a time-series extension with license strings attached.
sidebar:
  label: "0002 · PG partitioning"
  order: 2
---

**Status: accepted** (2026-08-15)

## Context

Telemetry is append-only time-series data, device metrics every 60
seconds. TimescaleDB is the obvious tool; declarative range partitioning
is the boring alternative.

## Decision

Daily range-partitioned plain PostgreSQL tables, with partitions created
and dropped by a dispatcher maintenance job. A non-partitioned
`device_status_latest` hot cache serves the dashboard.

## Rationale

- TimescaleDB's compelling features (compression, continuous aggregates)
  sit under the Timescale License, which is awkward inside an AGPL
  distribution and rules out the stock `postgres` image self-hosters
  expect.
- v1 volumes (1,000 devices is about 1.4M rows/day) are trivial for
  partitioned PostgreSQL.
- Retention is `DROP TABLE` on old partitions, with no vacuum churn.

## Consequences

- We own partition maintenance: a dispatcher scheduled job, with tests.
- If fleets reach a scale where compression matters, revisit. The ingest
  path is isolated in one module, so a storage swap stays contained.

Bitemporal inventory facts are unrelated to this choice: they are
regular tables with `tstzrange` columns. See the
[bitemporal concept page](/concepts/bitemporal/).
