import json
import uuid

from everwas.alerting.rules import CachedRule, rule_matches_device, value_for_metric
from everwas.ingest.heartbeat import parse_heartbeat
from everwas.models.alert import Metric, Operator, Severity


def _envelope(agent_id: str, data: dict) -> bytes:
    return json.dumps(
        {"v": 1, "type": "heartbeat", "agent_id": agent_id, "msg_id": "01X", "data": data}
    ).encode()


def test_parse_heartbeat_ok():
    agent_id = str(uuid.uuid4())
    parsed = parse_heartbeat(f"agents.{agent_id}.heartbeat", _envelope(agent_id, {"version": "1"}))
    assert parsed is not None
    got_id, data = parsed
    assert str(got_id) == agent_id
    assert data == {"version": "1"}


def test_parse_heartbeat_rejects_spoofed_agent_id():
    """Envelope agent_id must match the subject the agent was pinned to."""
    subject_id = str(uuid.uuid4())
    parsed = parse_heartbeat(f"agents.{subject_id}.heartbeat", _envelope(str(uuid.uuid4()), {}))
    assert parsed is None


def test_parse_heartbeat_rejects_garbage():
    assert parse_heartbeat("agents.not-a-uuid.heartbeat", b"{}") is None
    assert parse_heartbeat(f"agents.{uuid.uuid4()}.heartbeat", b"not json") is None
    assert parse_heartbeat("wrong.subject", b"{}") is None


def _rule(
    metric: Metric, operator=Operator.gt, threshold=90.0, target: dict | None = None
) -> CachedRule:
    # `is None`, not `or`: an empty target dict is meaningful (matches nothing)
    return CachedRule(
        id=uuid.uuid4(),
        name="test",
        metric=metric,
        operator=operator,
        threshold=threshold,
        duration_s=60,
        severity=Severity.warning,
        target={"all": True} if target is None else target,
        cooldown_s=300,
        channel_ids=(),
    )


def test_breach_respects_operator_direction():
    assert _rule(Metric.cpu, Operator.gt, 90).breached(95)
    assert not _rule(Metric.cpu, Operator.gt, 90).breached(85)
    assert _rule(Metric.disk, Operator.lt, 10).breached(5)
    assert not _rule(Metric.disk, Operator.lt, 10).breached(15)


def test_value_for_metric_derives_percentages():
    sample = {
        "cpu_pct": 42.5,
        "mem_used": 8 * 2**30,
        "mem_total": 16 * 2**30,
        "disks": [
            {"mount": "/", "used": 50, "total": 100},
            {"mount": "/data", "used": 91, "total": 100},
        ],
    }
    assert value_for_metric(Metric.cpu, sample) == 42.5
    assert value_for_metric(Metric.memory, sample) == 50.0
    # disk uses the WORST mount, not an average
    assert value_for_metric(Metric.disk, sample) == 91.0
    assert value_for_metric(Metric.heartbeat_missed, sample) is None


def test_value_for_metric_handles_missing_fields():
    assert value_for_metric(Metric.memory, {"mem_used": 1}) is None
    assert value_for_metric(Metric.disk, {"disks": []}) is None
    assert value_for_metric(Metric.cpu, {}) is None


class _Device:
    def __init__(self, device_id, tags):
        self.id = device_id
        self.tags = tags


def test_rule_targeting():
    device_id = uuid.uuid4()
    device = _Device(device_id, ["prod", "web"])

    assert rule_matches_device(_rule(Metric.cpu, target={"all": True}), device)
    assert rule_matches_device(_rule(Metric.cpu, target={"device_ids": [str(device_id)]}), device)
    assert rule_matches_device(_rule(Metric.cpu, target={"tags": ["web"]}), device)
    assert not rule_matches_device(_rule(Metric.cpu, target={"tags": ["db"]}), device)
    assert not rule_matches_device(
        _rule(Metric.cpu, target={"device_ids": [str(uuid.uuid4())]}), device
    )
    assert not rule_matches_device(_rule(Metric.cpu, target={}), device)
