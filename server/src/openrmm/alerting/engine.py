"""Threshold evaluation state machine.

Runs on telemetry ingest (sub-second alert latency, no polling). Per
(device, rule) we track when a breach began; an alert opens only once the
breach has persisted for the rule's duration. Recovery resolves it.

State is in memory: after a dispatcher restart a still-breaching device
re-arms and can fire up to one duration window late. That is an accepted
trade (documented in the plan) for keeping evaluation cheap.
"""

import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import select
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.alerting.rules import (
    CachedRule,
    RuleCache,
    rule_matches_device,
    value_for_metric,
)
from openrmm.models.alert import (
    Alert,
    AlertRule,
    AlertState,
    Metric,
    NotificationOutbox,
    OutboxStatus,
)
from openrmm.models.device import Device

log = structlog.get_logger()


@dataclass
class BreachState:
    since: datetime | None = None
    last_fired_at: datetime | None = None


@dataclass
class AlertEngine:
    rules: RuleCache = field(default_factory=RuleCache)
    _state: dict[tuple[uuid.UUID, uuid.UUID], BreachState] = field(default_factory=dict)

    def _entry(self, device_id: uuid.UUID, rule_id: uuid.UUID) -> BreachState:
        return self._state.setdefault((device_id, rule_id), BreachState())

    async def evaluate_telemetry(
        self, db: AsyncSession, device_id: uuid.UUID, sample: dict
    ) -> None:
        device = (
            await db.execute(select(Device).where(Device.id == device_id))
        ).scalar_one_or_none()
        if device is None:
            return

        now = datetime.now(UTC)
        for rule in await self.rules.get(db):
            if rule.metric in (Metric.heartbeat_missed, Metric.patch_overdue):
                continue  # not evaluated from telemetry samples
            if not rule_matches_device(rule, device):
                continue
            value = value_for_metric(rule.metric, sample)
            if value is None:
                continue

            state = self._entry(device_id, rule.id)
            if rule.breached(value):
                if state.since is None:
                    state.since = now
                elapsed = (now - state.since).total_seconds()
                if elapsed >= rule.duration_s:
                    await self._open(db, rule, device, value, now, state)
            else:
                state.since = None
                await self._resolve(db, rule.id, device_id, now)

    async def evaluate_heartbeat_missed(self, db: AsyncSession, devices: list[Device]) -> None:
        """Called by the offline sweep: absence of data can't fire on ingest."""
        now = datetime.now(UTC)
        for rule in await self.rules.get(db):
            if rule.metric != Metric.heartbeat_missed:
                continue
            for device in devices:
                if not rule_matches_device(rule, device):
                    continue
                state = self._entry(device.id, rule.id)
                state.since = state.since or now
                await self._open(db, rule, device, None, now, state)

    async def resolve_for_device(self, db: AsyncSession, device: Device) -> None:
        """Device came back: clear its heartbeat alerts.

        This queries open alerts directly instead of iterating the rule cache,
        because the cache only holds ENABLED rules. Resolving through the cache
        meant that disabling a rule stranded every alert it had opened, firing
        forever with no path back, which trains operators to ignore the alert
        list entirely.
        """
        now = datetime.now(UTC)
        open_alerts = (
            await db.execute(
                select(Alert.rule_id)
                .join(AlertRule, AlertRule.id == Alert.rule_id)
                .where(
                    Alert.device_id == device.id,
                    Alert.state != AlertState.resolved,
                    AlertRule.metric == Metric.heartbeat_missed,
                )
            )
        ).scalars()
        for rule_id in list(open_alerts):
            self._entry(device.id, rule_id).since = None
            await self._resolve(db, rule_id, device.id, now)

    async def _open(
        self,
        db: AsyncSession,
        rule: CachedRule,
        device: Device,
        value: float | None,
        now: datetime,
        state: BreachState,
    ) -> None:
        # Cooldown suppresses NOTIFICATIONS, never the alert row. Returning
        # here used to skip the INSERT entirely, so a second genuinely new
        # incident inside the cooldown left no record at all: the alerts table
        # was empty and the dashboard was indistinguishable from a healthy
        # fleet. The alert row is the durable incident record; a
        # notification-rate concern must not be able to erase it.
        in_cooldown = state.last_fired_at is not None and (
            now - state.last_fired_at < timedelta(seconds=rule.cooldown_s)
        )

        context = {
            "rule": rule.name,
            "metric": rule.metric.value,
            "operator": rule.operator.value,
            "threshold": rule.threshold,
            "duration_s": rule.duration_s,
            "hostname": device.hostname,
        }
        # The partial unique index (state <> 'resolved') makes this a no-op if
        # an alert is already open — dedup lives in the database, not here.
        stmt = (
            pg_insert(Alert.__table__)
            .values(
                id=uuid.uuid4(),
                rule_id=rule.id,
                device_id=device.id,
                state=AlertState.firing.value,
                severity=rule.severity.value,
                opened_at=now,
                last_value=value,
                context=context,
            )
            .on_conflict_do_nothing()
            .returning(Alert.__table__.c.id)
        )
        alert_id = (await db.execute(stmt)).scalar_one_or_none()
        if alert_id is None:
            return  # already firing

        state.last_fired_at = now
        title = f"{rule.name} on {device.hostname}"
        body = (
            f"{rule.metric.value} {rule.operator.value} {rule.threshold}"
            f" for {rule.duration_s}s" + (f" (current {value:.1f})" if value is not None else "")
        )
        if not in_cooldown:
            await self._enqueue(
                db, rule, alert_id, "alert.firing", title, body, device, value, context
            )
        log.info(
            "alert opened",
            rule=rule.name,
            device=device.hostname,
            severity=rule.severity.value,
            value=value,
            notified=not in_cooldown,
        )

    async def _resolve(
        self, db: AsyncSession, rule_id: uuid.UUID, device_id: uuid.UUID, now: datetime
    ) -> None:
        alert = (
            await db.execute(
                select(Alert).where(
                    Alert.rule_id == rule_id,
                    Alert.device_id == device_id,
                    Alert.state != AlertState.resolved,
                )
            )
        ).scalar_one_or_none()
        if alert is None:
            return
        alert.state = AlertState.resolved
        alert.resolved_at = now
        # A resolved incident ends the cooldown. Leaving last_fired_at set
        # meant the NEXT incident was measured against the previous one's fire
        # time, so a fresh problem could be silently suppressed.
        self._entry(device_id, rule_id).last_fired_at = None

        for rule in await self.rules.get(db):
            if rule.id != rule_id:
                continue
            hostname = alert.context.get("hostname", "device")
            await self._enqueue(
                db,
                rule,
                alert.id,
                "alert.resolved",
                f"RESOLVED: {rule.name} on {hostname}",
                f"{rule.metric.value} returned to normal",
                None,
                None,
                alert.context,
                hostname=hostname,
            )
        log.info("alert resolved", rule_id=str(rule_id), device_id=str(device_id))

    async def _enqueue(
        self,
        db: AsyncSession,
        rule: CachedRule,
        alert_id: uuid.UUID,
        kind: str,
        title: str,
        body: str,
        device: Device | None,
        value: float | None,
        context: dict,
        hostname: str | None = None,
    ) -> None:
        for channel_id in rule.channel_ids:
            db.add(
                NotificationOutbox(
                    alert_id=alert_id,
                    channel_id=channel_id,
                    status=OutboxStatus.pending,
                    payload={
                        "kind": kind,
                        "title": title,
                        "body": body,
                        "severity": rule.severity.value,
                        "device_hostname": device.hostname if device else hostname,
                        "alert_id": str(alert_id),
                        "context": {**context, "last_value": value},
                    },
                )
            )
