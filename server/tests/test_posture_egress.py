"""Posture egress: the envelope l2trace consumes, and its failure isolation.

Three properties carry the integration. The checks are passed through exactly
as the agent serialised them (the safe three-state form must not be re-derived
here); the envelope carries the join material and both clocks (MAC set with
loopbacks excluded, our ingest stamp for freshness, the endpoint's own stamp
as forensics); and a publish failure never fails the ingest it rides on.
"""

import json
from datetime import UTC, datetime

import pytest
from sqlalchemy import select

from everwas.db.engine import session_scope
from everwas.egress.posture import (
    PosturePublisher,
    current_macs,
    publish_posture,
    set_publisher,
)
from everwas.ingest.inventory import _facts_from
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.models.facts import FactPosture
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


@pytest.fixture(autouse=True)
def _no_publisher_leaks():
    """Egress is process-global state; a test must not configure the next one."""
    yield
    set_publisher(None)


class FakeNats:
    def __init__(self):
        self.published = []

    async def publish(self, subject, payload):
        self.published.append((subject, payload))


class BrokenNats:
    async def publish(self, subject, payload):
        raise ConnectionError("nats is down")


# A real collection, in the exact wire shape the agent's serialisation choke
# point emits: three statuses, reason as a field, forensic extras riding along.
CHECKS = [
    {
        "check": "antivirus",
        "category": "malware",
        "status": "not_assessed",
        "not_assessed_reason": "not_applicable",
        "detail": "resident antivirus is not part of the baseline on Linux",
    },
    {
        "check": "disk-encryption",
        "category": "encryption",
        "status": "fail",
        "detail": "the root filesystem is on unencrypted storage",
        "took_ms": 1,
    },
    {
        "check": "firewall",
        "category": "firewall",
        "status": "pass",
        "evidence": {"tool": "nftables"},
    },
]

INTERFACES = [
    {"name": "lo", "mac": "00:00:00:00:00:00", "loopback": True, "up": True},
    {"name": "enp0s2", "mac": "52:54:00:12:34:56", "loopback": False, "up": True},
    # A bridge sharing its member's MAC: the set must not repeat it.
    {"name": "ens4", "mac": "aa:bb:cc:dd:ee:ff", "loopback": False, "up": True},
    {"name": "br0", "mac": "aa:bb:cc:dd:ee:ff", "loopback": False, "up": True},
    # A tunnel with no hardware address contributes nothing.
    {"name": "tun0", "mac": "", "loopback": False, "up": True},
]


async def _seed_device(hostname: str):
    async with session_scope() as db:
        device = Device(
            id=uuid7(),
            hostname=hostname,
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
            agent_version="2026.08.20",
        )
        db.add(device)
        await db.flush()
        return device.id


async def _seed_network(device_id) -> None:
    from everwas.bitemporal.store import record_facts

    async with session_scope() as db:
        await record_facts(
            db, "network", device_id, _facts_from("network", {"interfaces": INTERFACES})
        )


def test_the_mac_set_excludes_loopbacks_and_dedupes():
    facts = [
        {"fact_key": f"iface:{i['name']}", "payload": i, "source": "agent"} for i in INTERFACES
    ]
    assert current_macs(facts) == ["52:54:00:12:34:56", "aa:bb:cc:dd:ee:ff"]


async def test_the_envelope_carries_identity_both_clocks_and_untouched_checks():
    device_id = await _seed_device("egress-host")
    await _seed_network(device_id)

    fake = FakeNats()
    set_publisher(PosturePublisher(fake, "l2trace.posture"))

    collected = datetime(2026, 8, 27, 12, 0, 0, tzinfo=UTC)
    before = datetime.now(UTC)
    await publish_posture(device_id, collected, {"checks": CHECKS})
    after = datetime.now(UTC)

    assert len(fake.published) == 1
    subject, raw = fake.published[0]
    assert subject == "l2trace.posture"

    envelope = json.loads(raw)
    assert envelope["device_id"] == str(device_id)
    assert envelope["hostname"] == "egress-host"
    assert envelope["agent_version"] == "2026.08.20"
    assert envelope["macs"] == ["52:54:00:12:34:56", "aa:bb:cc:dd:ee:ff"]

    # The endpoint's clock, verbatim, as forensic context.
    assert envelope["collected_at"] == "2026-08-27T12:00:00Z"
    # Our clock, stamped at publish. This is the one freshness gates on.
    ingested = datetime.fromisoformat(envelope["ingested_at"])
    assert before <= ingested <= after

    # Byte-for-byte passthrough of the agent's wire shape. Any transformation
    # here would re-open the four-status leak the serialisation choke point
    # closed, so equality is exact: statuses, reasons, categories, and the
    # forensic extras (detail, evidence, took_ms) all survive untouched.
    assert envelope["checks"] == CHECKS


async def test_ingest_survives_a_publish_failure():
    device_id = await _seed_device("egress-broken-peer")
    set_publisher(PosturePublisher(BrokenNats(), "l2trace.posture"))

    from everwas.dispatcher.consumers import _handle_inventory

    message = json.dumps(
        {
            "agent_id": str(device_id),
            "ts": datetime.now(UTC).isoformat(),
            "data": {"checks": CHECKS},
        }
    ).encode()

    # Must not raise: a raise here would nak and eventually dead-letter a
    # collection whose storage succeeded, because an egress peer was down.
    await _handle_inventory(f"agents.{device_id}.inventory.posture", message)

    async with session_scope() as db:
        stored = (
            (await db.execute(select(FactPosture).where(FactPosture.device_id == device_id)))
            .scalars()
            .all()
        )
    assert {f.fact_key for f in stored} == {
        "check:antivirus",
        "check:disk-encryption",
        "check:firewall",
    }, "the publish failure leaked into ingest"


async def test_no_publisher_configured_publishes_nothing_and_ingest_proceeds():
    # The default state: EVERWAS_POSTURE_EGRESS_SUBJECT unset, no publisher
    # installed. Posture ingest must behave exactly as it did before egress
    # existed.
    device_id = await _seed_device("egress-off")
    set_publisher(None)

    from everwas.dispatcher.consumers import _handle_inventory

    message = json.dumps(
        {
            "agent_id": str(device_id),
            "ts": datetime.now(UTC).isoformat(),
            "data": {"checks": CHECKS},
        }
    ).encode()
    await _handle_inventory(f"agents.{device_id}.inventory.posture", message)

    async with session_scope() as db:
        stored = (
            (await db.execute(select(FactPosture).where(FactPosture.device_id == device_id)))
            .scalars()
            .all()
        )
    assert len(stored) == 3


def test_the_egress_subject_defaults_to_off(monkeypatch):
    monkeypatch.delenv("EVERWAS_POSTURE_EGRESS_SUBJECT", raising=False)
    from everwas.config import Settings

    assert Settings().posture_egress_subject == ""


async def test_only_posture_collections_are_pushed():
    # The other inventory kinds flow through the same handler; none of them
    # belong on the verifier's subject.
    device_id = await _seed_device("egress-other-kinds")
    fake = FakeNats()
    set_publisher(PosturePublisher(fake, "l2trace.posture"))

    from everwas.dispatcher.consumers import _handle_inventory

    message = json.dumps(
        {
            "agent_id": str(device_id),
            "ts": datetime.now(UTC).isoformat(),
            "data": {"interfaces": INTERFACES},
        }
    ).encode()
    await _handle_inventory(f"agents.{device_id}.inventory.network", message)

    assert fake.published == []


async def test_an_unknown_device_is_skipped_not_raised():
    # A posture message for a device row that no longer exists (deleted
    # mid-flight) must not blow up the publish path.
    fake = FakeNats()
    set_publisher(PosturePublisher(fake, "l2trace.posture"))
    await publish_posture(uuid7(), datetime.now(UTC), {"checks": CHECKS})
    assert fake.published == []
