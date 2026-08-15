import json
import uuid

from openrmm.ingest.heartbeat import parse_heartbeat


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
