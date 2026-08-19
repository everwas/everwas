"""Threshold evaluation state machine.

Runs on telemetry ingest (sub-second alert latency, no polling). Per
(device, rule) we track when a breach began; an alert opens only once the
breach has persisted for the rule's duration. Recovery resolves it.

Two properties this module owes its caller:

1. **It cannot poison ingest.** Evaluation runs inside the same transaction as
   the telemetry insert, so an exception escaping here would roll back the
   sample and the message would redeliver forever. Alerting is important;
   telemetry persistence is critical. Everything below is wrapped so a bug in
   the important thing cannot destroy the critical one, and the alert writes
   happen inside a SAVEPOINT so a partial write cannot poison the outer
   transaction either.
2. **It reasons in SAMPLE time, not wall-clock time.** A delayed or redelivered
   sample carries the state of the world when it was taken. Evaluating it
   against now() let a stale non-breaching sample resolve a live alert and send
   a RESOLVED notification for a machine that was still on fire.

State is in memory: after a dispatcher restart a still-breaching device
re-arms and can fire up to one duration window late. That is an accepted
trade (documented in the plan) for keeping evaluation cheap. Where memory and
the database disagree, the database wins: cooldown is re-seeded from the open
alert's opened_at rather than trusted from memory.
"""

import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta

import structlog
from sqlalchemy import func, select, update
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from everwas.alerting.rules import (
    CachedRule,
    RuleCache,
    rule_matches_device,
    value_for_metric,
)
from everwas.models.alert import (
    Alert,
    AlertRule,
    AlertState,
    Metric,
    NotificationOutbox,
    OutboxStatus,
)
from everwas.models.device import Device, DeviceStatus

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

    def _snapshot(self) -> dict[tuple[uuid.UUID, uuid.UUID], BreachState]:
        return {key: BreachState(s.since, s.last_fired_at) for key, s in self._state.items()}

    def _restore(self, snapshot: dict[tuple[uuid.UUID, uuid.UUID], BreachState]) -> None:
        """Roll memory back to match a transaction that did not commit.

        In-memory state is mutated inside someone else's transaction. If that
        transaction rolls back, memory has moved on while the database has not,
        and the two disagree for the life of the process.
        """
        self._state = snapshot

    @asynccontextmanager
    async def _bulkhead(self, db: AsyncSession, what: str, **logctx: str) -> AsyncIterator[None]:
        """Contain a failure so it cannot reach the caller's transaction.

        Every public method here runs inside a transaction owned by ingest or
        the sweep. Two things have to be true: the alert writes must be
        all-or-nothing WITHIN that transaction (SAVEPOINT), and an exception
        must not escape (or the caller's own work rolls back and the message
        redelivers forever). In-memory state is rewound to match.
        """
        snapshot = self._snapshot()
        try:
            async with db.begin_nested():
                yield
        except Exception:
            self._restore(snapshot)
            # Swallowed deliberately: a missed alert must never cost a
            # telemetry sample or an offline sweep.
            log.exception(what, **logctx)

    async def evaluate_telemetry(
        self,
        db: AsyncSession,
        device_id: uuid.UUID,
        sample: dict,
        sample_ts: datetime | None = None,
    ) -> None:
        """Evaluate one telemetry sample. Never raises.

        `sample_ts` is when the sample was taken on the device. It defaults to
        now() so an older call site keeps working, but callers that have the
        wire timestamp should pass it: without it a redelivered sample from
        before an incident is evaluated as if it were current.
        """
        async with self._bulkhead(db, "alert evaluation failed", device_id=str(device_id)):
            await self._evaluate_telemetry(db, device_id, sample, sample_ts)

    async def _evaluate_telemetry(
        self,
        db: AsyncSession,
        device_id: uuid.UUID,
        sample: dict,
        sample_ts: datetime | None,
    ) -> None:
        now = _as_utc(sample_ts) or datetime.now(UTC)
        device = (
            await db.execute(
                select(Device).where(
                    Device.id == device_id,
                    # A retired device is not managed any more. Firing for it
                    # trains operators to ignore the alert list.
                    Device.status != DeviceStatus.retired,
                )
            )
        ).scalar_one_or_none()
        if device is None:
            return

        for rule in await self.rules.get(db):
            if rule.metric in (Metric.heartbeat_missed, Metric.patch_overdue):
                continue  # not evaluated from telemetry samples
            if not rule_matches_device(rule, device):
                continue
            value = value_for_metric(rule.metric, sample)
            if value is None:
                continue

            state = self._entry(device_id, rule.id)
            # An out-of-order sample describes a world older than the one this
            # state machine has already seen. It may not rewind it.
            if state.since is not None and now < state.since:
                log.debug(
                    "ignoring stale telemetry sample",
                    device_id=str(device_id),
                    rule=rule.name,
                    sample_ts=now.isoformat(),
                    breach_since=state.since.isoformat(),
                )
                continue

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
        """Called by the offline sweep: absence of data can't fire on ingest.

        Never raises: the sweep's own UPDATE (marking devices offline) shares
        this transaction and must survive an alerting bug.
        """
        async with self._bulkhead(db, "heartbeat alert evaluation failed"):
            await self._evaluate_heartbeat_missed(db, devices)

    async def _evaluate_heartbeat_missed(self, db: AsyncSession, devices: list[Device]) -> None:
        now = datetime.now(UTC)
        for rule in await self.rules.get(db):
            if rule.metric != Metric.heartbeat_missed:
                continue
            for device in devices:
                if not rule_matches_device(rule, device):
                    continue
                state = self._entry(device.id, rule.id)
                # Prefer the durable timestamp: last_heartbeat_at survives a
                # dispatcher restart, in-memory `since` does not.
                since = _as_utc(device.last_heartbeat_at) or state.since or now
                state.since = since
                # duration_s used to be ignored entirely here, so a rule
                # configured to wait an hour fired on the first sweep.
                if (now - since).total_seconds() < rule.duration_s:
                    continue
                await self._open(db, rule, device, None, now, state)

    async def resolve_for_device(self, db: AsyncSession, device: Device) -> None:
        """Device came back: clear its heartbeat alerts.

        This queries open alerts directly instead of iterating the rule cache,
        because the cache only holds ENABLED rules. Resolving through the cache
        meant that disabling a rule stranded every alert it had opened, firing
        forever with no path back, which trains operators to ignore the alert
        list entirely.

        Never raises: apply_heartbeat shares this transaction, and a device
        whose heartbeat was rolled back looks offline.
        """
        async with self._bulkhead(db, "heartbeat resolve failed", device_id=str(device.id)):
            await self._resolve_for_device(db, device)

    async def _resolve_for_device(self, db: AsyncSession, device: Device) -> None:
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
        if state.last_fired_at is None:
            # Re-seed from the database rather than trusting an empty memory.
            # Scoped to a STILL-OPEN alert on purpose: a resolved incident ends
            # the cooldown, so seeding from a resolved one would suppress the
            # notification for a genuinely new problem.
            state.last_fired_at = _as_utc(
                (
                    await db.execute(
                        select(func.max(Alert.opened_at)).where(
                            Alert.rule_id == rule.id,
                            Alert.device_id == device.id,
                            Alert.state != AlertState.resolved,
                        )
                    )
                ).scalar_one_or_none()
            )
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
            # Already firing. Refresh the reading anyway: leaving it at the
            # first breach meant the operator read "CPU 91%" for hours while
            # the machine was actually pinned at 100%.
            await db.execute(
                update(Alert.__table__)
                .where(
                    Alert.__table__.c.rule_id == rule.id,
                    Alert.__table__.c.device_id == device.id,
                    Alert.__table__.c.state != AlertState.resolved.value,
                )
                .values(last_value=value, context=context)
            )
            return

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
        opened_at = _as_utc(alert.opened_at)
        if opened_at is not None and now < opened_at:
            # The evidence predates the incident. Resolving on it would close a
            # live alert and mail an all-clear for a machine still on fire.
            log.warning(
                "refusing to resolve on evidence older than the alert",
                alert_id=str(alert.id),
                opened_at=opened_at.isoformat(),
                evidence_ts=now.isoformat(),
            )
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
        if not rule.channel_ids:
            # A rule with no channels produces an alert row nobody is told
            # about. The API refuses to create one; this covers the rules that
            # predate that check, and says so out loud.
            log.warning(
                "alert has no notification channels, nobody will be told",
                rule=rule.name,
                rule_id=str(rule.id),
                kind=kind,
            )
            return
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


def _as_utc(value: datetime | None) -> datetime | None:
    """Postgres hands back aware datetimes; callers might not. Normalise both."""
    if value is None:
        return None
    return value.replace(tzinfo=UTC) if value.tzinfo is None else value.astimezone(UTC)
