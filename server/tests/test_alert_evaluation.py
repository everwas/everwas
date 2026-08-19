"""Alert evaluation against a real database.

Two properties dominate this file:

- Evaluation must not be able to destroy telemetry. It runs inside the ingest
  transaction, so an exception escaping it rolls back the sample and the
  message redelivers forever. One agent sending a string where a number
  belongs used to stop that device's history being written, permanently.
- Evaluation must reason in SAMPLE time. A delayed or redelivered sample
  carries an old world; judged against now() it resolved live alerts and
  mailed all-clears for machines that were still on fire.
"""

import math
import uuid

import pytest
from sqlalchemy import delete, select

from everwas.alerting import engine as engine_mod
from everwas.alerting.engine import AlertEngine
from everwas.alerting.rules import numeric, value_for_metric
from everwas.db.engine import get_sessionmaker
from everwas.models.alert import (
    Alert,
    AlertRule,
    AlertState,
    ChannelKind,
    Metric,
    NotificationChannel,
    NotificationOutbox,
    Operator,
    RuleChannel,
    Severity,
)
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.util.ids import uuid7

# --- value_for_metric: the boundary that keeps junk out of the comparison ---


def test_numeric_rejects_everything_that_is_not_a_finite_number():
    assert numeric(42) == 42.0
    assert numeric(3.5) == 3.5
    assert numeric(0) == 0.0
    for junk in ("high", None, [1], {"a": 1}, True, False, math.nan, math.inf, -math.inf):
        assert numeric(junk) is None, junk


def test_value_for_metric_refuses_junk_rather_than_handing_it_to_the_comparison():
    # Each of these reached `"high" > 90.0` and raised TypeError inside the
    # ingest transaction.
    assert value_for_metric(Metric.cpu, {"cpu_pct": "high"}) is None
    assert value_for_metric(Metric.cpu, {"cpu_pct": None}) is None
    assert value_for_metric(Metric.cpu, {"cpu_pct": True}) is None
    assert value_for_metric(Metric.cpu, {"cpu_pct": float("nan")}) is None
    assert value_for_metric(Metric.memory, {"mem_used": "8G", "mem_total": 16}) is None
    assert value_for_metric(Metric.memory, {"mem_used": 8, "mem_total": 0}) is None
    assert value_for_metric(Metric.disk, {"disks": "sda"}) is None
    assert value_for_metric(Metric.disk, {"disks": [{"used": "a lot", "total": 100}]}) is None
    assert value_for_metric(Metric.cpu, "not a sample") is None


def test_value_for_metric_still_reads_real_numbers():
    assert value_for_metric(Metric.cpu, {"cpu_pct": 96.4}) == 96.4
    assert value_for_metric(Metric.memory, {"mem_used": 4, "mem_total": 16}) == 25.0
    # a mount at 0% used is a measurement, not a missing field
    assert value_for_metric(Metric.disk, {"disks": [{"used": 0, "total": 100}]}) == 0.0


# --- everything below needs the database ------------------------------------

pytestmark = pytest.mark.usefixtures("pg_database")


async def seed(
    *,
    metric: Metric = Metric.cpu,
    operator: Operator = Operator.gt,
    threshold: float = 90.0,
    duration_s: int = 0,
    cooldown_s: int = 900,
    device_status: DeviceStatus = DeviceStatus.active,
    last_heartbeat_at=None,
    with_channel: bool = True,
) -> tuple[Device, AlertRule, NotificationChannel]:
    device = Device(
        id=uuid7(),
        hostname="web-01",
        os_family=OsFamily.linux,
        status=device_status,
        tags=["prod"],
        last_heartbeat_at=last_heartbeat_at,
    )
    rule = AlertRule(
        id=uuid.uuid4(),
        name=f"rule-{uuid.uuid4().hex[:8]}",
        metric=metric,
        operator=operator,
        threshold=threshold,
        duration_s=duration_s,
        severity=Severity.critical,
        target={"all": True},
        cooldown_s=cooldown_s,
        enabled=True,
    )
    channel = NotificationChannel(
        id=uuid.uuid4(),
        name=f"ch-{uuid.uuid4().hex[:8]}",
        kind=ChannelKind.webhook,
        config={"url": "https://hooks.example.com/rmm"},
        enabled=True,
    )
    async with get_sessionmaker()() as db, db.begin():
        db.add_all([device, rule, channel])
        await db.flush()
        if with_channel:
            db.add(RuleChannel(rule_id=rule.id, channel_id=channel.id))
    return device, rule, channel


async def alerts_for(rule_id: uuid.UUID) -> list[Alert]:
    async with get_sessionmaker()() as db:
        return list(
            (
                await db.execute(
                    select(Alert).where(Alert.rule_id == rule_id).order_by(Alert.opened_at)
                )
            ).scalars()
        )


async def outbox_kinds() -> list[str]:
    async with get_sessionmaker()() as db:
        rows = (await db.execute(select(NotificationOutbox))).scalars()
        return [row.payload.get("kind") for row in rows]


# --- H2: evaluation cannot poison the ingest transaction --------------------


@pytest.mark.parametrize(
    "sample",
    [
        {"cpu_pct": "high"},
        {"cpu_pct": [99]},
        {"cpu_pct": {"value": 99}},
        {"cpu_pct": "99"},
    ],
)
async def test_a_junk_metric_value_cannot_poison_the_ingest_transaction(sample):
    """`"high" > 90.0` raised TypeError, rolling the telemetry sample back with it."""
    device, rule, _ = await seed()
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, sample)
        # The caller's transaction is still usable. This is the property that
        # decides whether that device ever gets telemetry history again.
        await db.execute(select(Alert))
    assert await alerts_for(rule.id) == []


async def test_a_boolean_is_not_a_measurement():
    """bool is an int subclass, so True used to compare as 1.0 and fire."""
    device, rule, _ = await seed(operator=Operator.lt, threshold=90.0)
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": True})
    assert await alerts_for(rule.id) == []


async def test_a_failed_alert_write_does_not_abort_the_callers_transaction():
    """The SAVEPOINT. A broken alert write must cost the alert, not the sample.

    The failure here is real, not mocked: the rule cache still holds a channel
    id the operator has since deleted, so the outbox INSERT violates its
    foreign key.
    """
    device, rule, channel = await seed()
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.rules.get(db)  # warm the cache while the channel exists
    async with get_sessionmaker()() as db, db.begin():
        await db.execute(delete(NotificationChannel).where(NotificationChannel.id == channel.id))

    marker = Device(id=uuid7(), hostname="marker-01", os_family=OsFamily.linux)
    async with get_sessionmaker()() as db, db.begin():
        # stands in for the telemetry insert that shares this transaction
        db.add(marker)
        await db.flush()
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99})

    async with get_sessionmaker()() as db:
        survived = (
            await db.execute(select(Device).where(Device.id == marker.id))
        ).scalar_one_or_none()
    assert survived is not None, "the caller's write was rolled back by an alerting failure"


async def test_an_unexpected_bug_in_evaluation_is_contained(monkeypatch):
    device, rule, _ = await seed()

    def explode(*_args, **_kwargs):
        raise RuntimeError("targeting is broken")

    monkeypatch.setattr(engine_mod, "rule_matches_device", explode)
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99})
        await db.execute(select(Alert))
    assert await alerts_for(rule.id) == []


# --- H3: sample time, not wall-clock time -----------------------------------


async def test_a_stale_sample_does_not_resolve_a_live_alert():
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed(cooldown_s=0)
    engine = AlertEngine()
    t0 = datetime.now(UTC)

    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0)
    (alert,) = await alerts_for(rule.id)
    assert alert.state == AlertState.firing

    # A redelivered sample from before the incident. The machine is still on
    # fire; this is just old evidence arriving late.
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 5}, t0 - timedelta(minutes=10))

    (alert,) = await alerts_for(rule.id)
    assert alert.state == AlertState.firing, "a stale sample resolved a live alert"
    assert "alert.resolved" not in await outbox_kinds(), "an all-clear went out for a live incident"


async def test_a_stale_sample_does_not_resolve_after_a_dispatcher_restart():
    """The in-memory guard is gone after a restart; opened_at is the backstop."""
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed(cooldown_s=0)
    t0 = datetime.now(UTC)
    async with get_sessionmaker()() as db, db.begin():
        await AlertEngine().evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0)

    restarted = AlertEngine()  # empty memory, open alert in the database
    async with get_sessionmaker()() as db, db.begin():
        await restarted.evaluate_telemetry(
            db, device.id, {"cpu_pct": 5}, t0 - timedelta(minutes=10)
        )

    (alert,) = await alerts_for(rule.id)
    assert alert.state == AlertState.firing
    assert alert.resolved_at is None


async def test_a_current_sample_still_resolves():
    """The staleness guard must not break recovery."""
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed(cooldown_s=0)
    engine = AlertEngine()
    t0 = datetime.now(UTC)
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0)
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 5}, t0 + timedelta(seconds=30))

    (alert,) = await alerts_for(rule.id)
    assert alert.state == AlertState.resolved
    assert "alert.resolved" in await outbox_kinds()


async def test_duration_is_measured_in_sample_time():
    """Two samples 10 minutes apart on the wire satisfy a 5 minute duration."""
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed(duration_s=300)
    engine = AlertEngine()
    t0 = datetime.now(UTC) - timedelta(hours=1)
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0)
    assert await alerts_for(rule.id) == [], "fired before the duration elapsed"
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0 + timedelta(minutes=10))
    assert len(await alerts_for(rule.id)) == 1


# --- M-tier ------------------------------------------------------------------


async def test_the_reading_is_refreshed_while_the_alert_stays_open():
    """ "CPU 91%" for hours while the machine sits at 100% is a lie."""
    device, rule, _ = await seed()
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 91})
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 100})

    (alert,) = await alerts_for(rule.id)
    assert float(alert.last_value) == 100.0


async def test_a_retired_device_does_not_fire():
    device, rule, _ = await seed(device_status=DeviceStatus.retired)
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99})
    assert await alerts_for(rule.id) == []


async def test_heartbeat_missed_honours_the_rules_duration():
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed(
        metric=Metric.heartbeat_missed,
        duration_s=3600,
        device_status=DeviceStatus.offline,
        last_heartbeat_at=datetime.now(UTC) - timedelta(minutes=2),
    )
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        offline = list((await db.execute(select(Device).where(Device.id == device.id))).scalars())
        await engine.evaluate_heartbeat_missed(db, offline)
    assert await alerts_for(rule.id) == [], "fired an hour early"

    async with get_sessionmaker()() as db, db.begin():
        offline = list((await db.execute(select(Device).where(Device.id == device.id))).scalars())
        offline[0].last_heartbeat_at = datetime.now(UTC) - timedelta(hours=2)
        await engine.evaluate_heartbeat_missed(db, offline)
    assert len(await alerts_for(rule.id)) == 1


async def test_cooldown_is_reseeded_from_the_open_alert_after_a_restart():
    """Memory says "never fired"; the database says an alert is open. The
    database wins, or the two disagree for the life of the process."""
    from datetime import UTC, datetime, timedelta

    device, rule, _ = await seed()
    t0 = datetime.now(UTC)
    async with get_sessionmaker()() as db, db.begin():
        await AlertEngine().evaluate_telemetry(db, device.id, {"cpu_pct": 99}, t0)

    restarted = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await restarted.evaluate_telemetry(
            db, device.id, {"cpu_pct": 99}, t0 + timedelta(seconds=5)
        )
    state = restarted._state[(device.id, rule.id)]
    assert state.last_fired_at is not None


async def test_a_rule_with_no_channels_still_records_the_alert():
    """Nobody is told, but the incident is not invisible too."""
    device, rule, _ = await seed(with_channel=False)
    engine = AlertEngine()
    async with get_sessionmaker()() as db, db.begin():
        await engine.evaluate_telemetry(db, device.id, {"cpu_pct": 99})
    assert len(await alerts_for(rule.id)) == 1
    assert await outbox_kinds() == []
