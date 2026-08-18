"""Security posture arrives as one bitemporal fact per check.

Per check rather than per machine, because the set of checks grows. A machine
assessed last month was assessed against last month's checks, and a check added
since is not one that machine failed, it is one that never ran on it. Only
per-check storage can say that.
"""

import pytest

from openrmm.db.engine import session_scope
from openrmm.ingest.inventory import _facts_from
from openrmm.models.device import Device, DeviceStatus, OsFamily
from openrmm.models.facts import FACT_TABLES, FactKind, FactPosture
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


def _snapshot(*results: dict) -> dict:
    return {"checks": list(results)}


def test_each_check_becomes_its_own_fact():
    facts = _facts_from(
        "posture",
        _snapshot(
            {
                "check": "disk-encryption",
                "category": "encryption",
                "status": "fail",
                "detail": "plaintext root",
            },
            {"check": "firewall", "category": "firewall", "status": "pass"},
            {
                "check": "antivirus",
                "category": "malware",
                "status": "not_assessed",
                "not_assessed_reason": "not_applicable",
            },
        ),
    )
    assert set(facts) == {"check:disk-encryption", "check:firewall", "check:antivirus"}
    # The key is not repeated inside the payload it identifies.
    assert "check" not in facts["check:firewall"]
    assert facts["check:disk-encryption"]["status"] == "fail"
    # The category rides along, so a site policy can gate on "encryption"
    # rather than enumerating every encryption check by name.
    assert facts["check:disk-encryption"]["category"] == "encryption"


def test_a_check_with_no_name_is_dropped_rather_than_keyed_as_nothing():
    # A malformed result from a broken agent build must not produce a fact
    # keyed "check:" that then amends itself on every cycle.
    facts = _facts_from("posture", _snapshot({"status": "pass"}, {"check": "ok", "status": "pass"}))
    assert set(facts) == {"check:ok"}


def test_an_empty_posture_produces_no_facts():
    # Distinct from "everything passed". The agent is supposed to skip
    # publishing entirely on a platform with no checks, and if one ever does
    # publish an empty set it must not read as a clean bill of health.
    assert _facts_from("posture", {"checks": []}) == {}
    assert _facts_from("posture", {}) == {}


def test_posture_is_a_known_kind_everywhere_it_needs_to_be():
    # These two drifted once before, for "network" and "logins", and the only
    # symptom was a 422 in the browser for a kind the server stored perfectly
    # well. The import-time assertion catches the pair; this catches the table.
    assert "posture" in FACT_TABLES
    assert "posture" in FactKind.__args__
    assert FACT_TABLES["posture"] is FactPosture


async def test_the_posture_table_stores_and_reads_back():
    # Proves the migration actually created a usable table, which a unit test
    # on the key derivation alone would not.
    async with session_scope() as db:
        device = Device(
            id=uuid7(),
            hostname="posture-host",
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
        )
        db.add(device)
        await db.flush()
        device_id = device.id

    from openrmm.bitemporal.store import record_facts

    facts = _facts_from(
        "posture",
        _snapshot(
            {"check": "firewall", "status": "pass"},
            {"check": "disk-encryption", "status": "fail"},
        ),
    )
    async with session_scope() as db:
        await record_facts(db, "posture", device_id, facts)

    async with session_scope() as db:
        from sqlalchemy import select

        rows = (
            (await db.execute(select(FactPosture).where(FactPosture.device_id == device_id)))
            .scalars()
            .all()
        )
        stored = {r.fact_key: r.payload for r in rows}

    assert stored["check:firewall"]["status"] == "pass"
    assert stored["check:disk-encryption"]["status"] == "fail"


async def test_a_check_added_later_has_no_history_before_it_existed():
    # The property the per-check design exists for. A machine assessed before a
    # check was written must not acquire a retroactive verdict for it.
    async with session_scope() as db:
        device = Device(
            id=uuid7(),
            hostname="posture-history",
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
        )
        db.add(device)
        await db.flush()
        device_id = device.id

    from openrmm.bitemporal.store import record_facts

    # First assessment: only the firewall check existed.
    async with session_scope() as db:
        await record_facts(
            db,
            "posture",
            device_id,
            _facts_from("posture", _snapshot({"check": "firewall", "status": "pass"})),
        )

    # Later, disk encryption is added and both run.
    async with session_scope() as db:
        await record_facts(
            db,
            "posture",
            device_id,
            _facts_from(
                "posture",
                _snapshot(
                    {"check": "firewall", "status": "pass"},
                    {"check": "disk-encryption", "status": "fail"},
                ),
            ),
        )

    async with session_scope() as db:
        from sqlalchemy import select

        rows = (
            (
                await db.execute(
                    select(FactPosture).where(
                        FactPosture.device_id == device_id,
                        FactPosture.fact_key == "check:disk-encryption",
                    )
                )
            )
            .scalars()
            .all()
        )

    assert len(rows) == 1, (
        "the newly added check acquired more than one belief, so the machine "
        "was given a history for a check that had never run on it"
    )


def test_an_unknown_kind_is_still_refused():
    # The dispatch is a chain of ifs, so a new branch that fell through would
    # silently accept anything.
    with pytest.raises(ValueError):
        _facts_from("not-a-kind", {})


# --- routing: FACT_KINDS membership is what makes posture real -------------------


def test_the_posture_subject_is_accepted_at_the_boundary():
    # parse_inventory refuses subjects for kinds it does not know. _facts_from
    # knowing how to flatten posture is worthless if the subject never gets
    # that far — which is exactly what happened while "posture" was handled in
    # the flattener but missing from FACT_KINDS: the agent published, the
    # subject was dropped, and no fact ever appeared.
    import json
    from datetime import UTC, datetime

    from openrmm.ingest.inventory import parse_inventory

    agent_id = uuid7()
    envelope = json.dumps(
        {
            "agent_id": str(agent_id),
            "ts": datetime.now(UTC).isoformat(),
            "data": _snapshot({"check": "firewall", "status": "pass"}),
        }
    ).encode()
    parsed = parse_inventory(f"agents.{agent_id}.inventory.posture", envelope)
    assert parsed is not None
    parsed_agent, kind, _, data = parsed
    assert parsed_agent == agent_id
    assert kind == "posture"
    assert data["checks"][0]["check"] == "firewall"


async def test_apply_inventory_routes_posture_to_facts_not_snapshots():
    # The other half of the same routing: a kind outside FACT_KINDS falls
    # through to the latest-only snapshot store, which keeps no history and
    # feeds no sweep. Posture must land in fact_posture.
    from datetime import UTC, datetime

    from sqlalchemy import select

    from openrmm.ingest.inventory import apply_inventory
    from openrmm.models.telemetry import DeviceSnapshot

    async with session_scope() as db:
        device = Device(
            id=uuid7(),
            hostname="posture-routing",
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
        )
        db.add(device)
        await db.flush()
        device_id = device.id

    async with session_scope() as db:
        await apply_inventory(
            db,
            device_id,
            "posture",
            datetime.now(UTC),
            _snapshot({"check": "firewall", "status": "pass"}),
        )

    async with session_scope() as db:
        facts = (
            (await db.execute(select(FactPosture).where(FactPosture.device_id == device_id)))
            .scalars()
            .all()
        )
        snapshots = (
            (
                await db.execute(
                    select(DeviceSnapshot).where(
                        DeviceSnapshot.device_id == device_id, DeviceSnapshot.kind == "posture"
                    )
                )
            )
            .scalars()
            .all()
        )
    assert [f.fact_key for f in facts] == ["check:firewall"]
    assert snapshots == [], "posture was misrouted to the latest-only snapshot store"
