"""Daily partition maintenance for the telemetry tables.

Creates partitions ahead of time and drops those past retention. Runs at
dispatcher startup and then daily.
"""

from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.models.telemetry import PARTITIONED_TELEMETRY

log = structlog.get_logger()

# Derived from the model definitions, so a new partitioned table cannot be
# created by a migration and then silently never get partitions or retention.
PARTITIONED_TABLES = tuple(t.name for t in PARTITIONED_TELEMETRY)
CREATE_AHEAD_DAYS = 2


def _pname(table: str, day: datetime) -> str:
    return f"{table}_p{day:%Y%m%d}"


async def ensure_partitions(db: AsyncSession, retention_days: int) -> None:
    today = datetime.now(UTC).replace(hour=0, minute=0, second=0, microsecond=0)

    for table in PARTITIONED_TABLES:
        for offset in range(-1, CREATE_AHEAD_DAYS + 1):
            day = today + timedelta(days=offset)
            await db.execute(
                text(
                    f"CREATE TABLE IF NOT EXISTS {_pname(table, day)} "
                    f"PARTITION OF {table} "
                    f"FOR VALUES FROM ('{day:%Y-%m-%d}') TO ('{day + timedelta(days=1):%Y-%m-%d}')"
                )
            )

        # retention: drop partitions entirely older than the cutoff
        cutoff = today - timedelta(days=retention_days)
        rows = await db.execute(
            text(
                "SELECT c.relname FROM pg_class c "
                "JOIN pg_inherits i ON i.inhrelid = c.oid "
                "JOIN pg_class p ON p.oid = i.inhparent "
                "WHERE p.relname = :parent"
            ),
            {"parent": table},
        )
        for (name,) in rows:
            try:
                day = datetime.strptime(name.rsplit("_p", 1)[1], "%Y%m%d").replace(tzinfo=UTC)
            except (IndexError, ValueError):
                continue
            if day < cutoff:
                await db.execute(text(f"DROP TABLE IF EXISTS {name}"))
                log.info("partition dropped", partition=name)
