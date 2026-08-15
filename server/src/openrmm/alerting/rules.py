"""Rule cache and target matching.

Rules change rarely and are read on every telemetry sample, so they're cached
in the dispatcher and invalidated explicitly (the API bumps a version row via
LISTEN/NOTIFY; until then a periodic refresh keeps it honest).
"""

import time
import uuid
from dataclasses import dataclass

import structlog
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.alert import AlertRule, Metric, Operator, RuleChannel, Severity
from openrmm.models.device import Device

log = structlog.get_logger()

CACHE_TTL_S = 30


@dataclass(frozen=True)
class CachedRule:
    id: uuid.UUID
    name: str
    metric: Metric
    operator: Operator
    threshold: float
    duration_s: int
    severity: Severity
    target: dict
    cooldown_s: int
    channel_ids: tuple[uuid.UUID, ...]

    def breached(self, value: float) -> bool:
        if self.operator == Operator.gt:
            return value > self.threshold
        return value < self.threshold


class RuleCache:
    def __init__(self) -> None:
        self._rules: list[CachedRule] = []
        self._loaded_at: float = 0.0

    def invalidate(self) -> None:
        self._loaded_at = 0.0

    async def get(self, db: AsyncSession) -> list[CachedRule]:
        if time.monotonic() - self._loaded_at < CACHE_TTL_S:
            return self._rules

        rows = (await db.execute(select(AlertRule).where(AlertRule.enabled.is_(True)))).scalars()
        # .scalars(): select(Model) yields Row tuples otherwise
        links = (await db.execute(select(RuleChannel))).scalars()
        by_rule: dict[uuid.UUID, list[uuid.UUID]] = {}
        for link in links:
            by_rule.setdefault(link.rule_id, []).append(link.channel_id)

        self._rules = [
            CachedRule(
                id=r.id,
                name=r.name,
                metric=r.metric,
                operator=r.operator,
                threshold=float(r.threshold),
                duration_s=r.duration_s,
                severity=r.severity,
                target=r.target or {},
                cooldown_s=r.cooldown_s,
                channel_ids=tuple(by_rule.get(r.id, [])),
            )
            for r in rows
        ]
        self._loaded_at = time.monotonic()
        log.debug("rule cache refreshed", rules=len(self._rules))
        return self._rules


def rule_matches_device(rule: CachedRule, device: Device) -> bool:
    target = rule.target
    if target.get("all"):
        return True
    if device_ids := target.get("device_ids"):
        return str(device.id) in {str(d) for d in device_ids}
    if tags := target.get("tags"):
        return bool(set(device.tags or []) & set(tags))
    return False


def value_for_metric(metric: Metric, sample: dict) -> float | None:
    """Pull the comparable value for a metric out of a telemetry sample."""
    if metric == Metric.cpu:
        return sample.get("cpu_pct")
    if metric == Metric.memory:
        used, total = sample.get("mem_used"), sample.get("mem_total")
        return (used / total * 100.0) if used and total else None
    if metric == Metric.disk:
        pcts = [
            d["used"] / d["total"] * 100.0
            for d in (sample.get("disks") or [])
            if d.get("used") and d.get("total")
        ]
        return max(pcts) if pcts else None
    return None
