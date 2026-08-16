"""Rule cache and target matching.

Rules change rarely and are read on every telemetry sample, so they're cached
in the dispatcher. There is NO push invalidation: the only thing that refreshes
the cache is CACHE_TTL_S expiring (or an in-process invalidate() call), so a
rule edited through the API takes up to that long to take effect in the
dispatcher. An earlier version of this docstring claimed the API pushed an
invalidation via LISTEN/NOTIFY. No such mechanism was ever written, and
believing it would have meant trusting a rule change to apply instantly.
"""

import math
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


def numeric(value: object) -> float | None:
    """A telemetry field is only comparable if it is a finite real number.

    The agent's payload is attacker-adjacent data, not a trusted struct. An
    agent sending {"cpu_pct": "high"} used to reach rule.breached(), where
    "high" > 90.0 raises TypeError inside the ingest transaction and takes the
    telemetry sample down with it, permanently, on every redelivery. bool is
    excluded on purpose: it is an int subclass, so True would silently compare
    as 1.0 and read as a real measurement.
    """
    if isinstance(value, bool) or not isinstance(value, int | float):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def _ratio_pct(used: object, total: object) -> float | None:
    used_n, total_n = numeric(used), numeric(total)
    if used_n is None or total_n is None or total_n <= 0:
        return None
    return used_n / total_n * 100.0


def value_for_metric(metric: Metric, sample: dict) -> float | None:
    """Pull the comparable value for a metric out of a telemetry sample.

    Returns None for anything that is missing, the wrong type, or not finite.
    Callers may assume the result is safe to compare against a threshold.
    """
    if not isinstance(sample, dict):
        return None
    if metric == Metric.cpu:
        return numeric(sample.get("cpu_pct"))
    if metric == Metric.memory:
        return _ratio_pct(sample.get("mem_used"), sample.get("mem_total"))
    if metric == Metric.disk:
        disks = sample.get("disks")
        if not isinstance(disks, list):
            return None
        pcts = [
            pct
            for d in disks
            if isinstance(d, dict)
            and (pct := _ratio_pct(d.get("used"), d.get("total"))) is not None
        ]
        return max(pcts) if pcts else None
    return None
