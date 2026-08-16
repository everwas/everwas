"""Server-side schedules: the document, the version, and recording the runs.

The agent's scheduler has been complete since M3 and has never fired once,
because nothing ever sent it a document. These cover the three pieces that
were missing: building what the agent parses, deciding when a device is out of
step, and recording a run the server never queued.
"""

import json
import uuid
from datetime import UTC, datetime

import pytest

from openrmm.db.engine import get_sessionmaker
from openrmm.ingest.results import apply_job_output, apply_job_result
from openrmm.models.device import Device, OsFamily
from openrmm.models.script import (
    RunStatus,
    RunTrigger,
    Script,
    ScriptRun,
    ScriptSchedule,
    ShellKind,
)
from openrmm.services.schedules import (
    build_document,
    load_schedules,
    schedule_version,
    scheduled_job_id,
)
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _fixture(target: dict | None = None, tags: list[str] | None = None):
    async with get_sessionmaker()() as db, db.begin():
        device = Device(
            id=uuid7(), hostname="sched-host", os_family=OsFamily.linux, tags=tags or []
        )
        script = Script(
            name=f"nightly-{uuid.uuid4().hex[:6]}",
            shell=ShellKind.bash,
            body="echo hi",
            sha256="0" * 64,
            timeout_s=120,
        )
        db.add_all([device, script])
        await db.flush()
        schedule = ScriptSchedule(
            name="nightly",
            script_id=script.id,
            cron="0 2 * * *",
            tz="America/Denver",
            target=target if target is not None else {"all": True},
            jitter_s=300,
            misfire_grace_s=3600,
        )
        db.add(schedule)
    return device, script, schedule


async def test_the_document_matches_what_the_agent_parses():
    """Every field here is read by sched.Entry in Go. A missing one is an
    entry the agent silently drops or mis-schedules."""
    device, script, schedule = await _fixture()
    async with get_sessionmaker()() as db:
        doc = build_document(device, await load_schedules(db))

    assert set(doc) == {"schedule_version", "entries"}
    (entry,) = doc["entries"]
    assert set(entry) == {
        "entry_id",
        "cron",
        "tz",
        "kind",
        "payload",
        "jitter_s",
        "misfire_grace_s",
        "enabled",
    }
    assert entry["entry_id"] == str(schedule.id)
    assert entry["cron"] == "0 2 * * *"
    assert entry["tz"] == "America/Denver"
    assert entry["kind"] == "script.run"
    assert entry["payload"]["body"] == "echo hi"
    assert entry["payload"]["shell"] == "bash"
    assert entry["payload"]["timeout_s"] == 120


async def test_the_payload_follows_the_script():
    """Built from the script every time, not snapshotted onto the schedule.
    Otherwise editing a script leaves its schedules running whatever the body
    was on the day they were made."""
    device, script, _schedule = await _fixture()
    async with get_sessionmaker()() as db, db.begin():
        (await db.get(Script, script.id)).body = "echo CHANGED"

    async with get_sessionmaker()() as db:
        doc = build_document(device, await load_schedules(db))
    assert doc["entries"][0]["payload"]["body"] == "echo CHANGED"


async def test_a_device_the_schedule_does_not_target_gets_nothing():
    device, _script, _schedule = await _fixture(target={"tags": ["servers"]}, tags=["laptops"])
    async with get_sessionmaker()() as db:
        doc = build_document(device, await load_schedules(db))
    assert doc["entries"] == []
    assert doc["schedule_version"] == schedule_version([])


async def test_the_version_is_the_content():
    """It is compared against what the agent reports on every heartbeat. A
    counter would drift across restarts and resync the fleet for nothing; a
    content hash recomputes to the same answer for ever."""
    device, _script, _schedule = await _fixture()
    async with get_sessionmaker()() as db:
        first = build_document(device, await load_schedules(db))
        second = build_document(device, await load_schedules(db))
    assert first["schedule_version"] == second["schedule_version"]
    assert 0 < first["schedule_version"] < 2**31


async def test_changing_a_schedule_changes_the_version():
    device, _script, schedule = await _fixture()
    async with get_sessionmaker()() as db:
        before = build_document(device, await load_schedules(db))["schedule_version"]

    async with get_sessionmaker()() as db, db.begin():
        (await db.get(ScriptSchedule, schedule.id)).cron = "30 3 * * *"

    async with get_sessionmaker()() as db:
        after = build_document(device, await load_schedules(db))["schedule_version"]
    assert before != after


async def test_a_disabled_schedule_is_absent_not_disabled():
    """Sending enabled=false would make the agent hold an entry it will never
    fire, and would change the version of every device the schedule does not
    even target."""
    device, _script, schedule = await _fixture()
    async with get_sessionmaker()() as db, db.begin():
        (await db.get(ScriptSchedule, schedule.id)).enabled = False

    async with get_sessionmaker()() as db:
        doc = build_document(device, await load_schedules(db))
    assert doc["entries"] == []


def _envelope(agent_id: uuid.UUID, data: dict) -> bytes:
    return json.dumps({"v": 1, "agent_id": str(agent_id), "data": data}).encode()


async def test_a_scheduled_result_is_recorded():
    """The gap this closes: a scheduled fire comes out of the agent's own
    cache, so no server ever queued it or wrote a row. Its result used to be
    logged as an unknown run and thrown away, so a nightly job could run
    correctly for a month with no record anywhere."""
    device, script, schedule = await _fixture()
    fire_at = datetime(2026, 8, 16, 8, 0, tzinfo=UTC)
    job_id = scheduled_job_id(str(schedule.id), fire_at)

    async with get_sessionmaker()() as db, db.begin():
        await apply_job_result(
            db,
            device.id,
            job_id,
            {"status": "succeeded", "exit_code": 0, "entry_id": str(schedule.id)},
        )

    async with get_sessionmaker()() as db:
        run = await db.get(ScriptRun, job_id)
    assert run is not None, "the scheduled run was dropped"
    assert run.status is RunStatus.succeeded
    assert run.trigger is RunTrigger.schedule
    assert run.script_id == script.id
    assert run.device_id == device.id


async def test_scheduled_output_arrives_before_the_result_and_is_kept():
    """Output streams during the run; the result comes last. Waiting for the
    result to create the row loses the whole of the job's stdout."""
    device, _script, schedule = await _fixture()
    job_id = scheduled_job_id(str(schedule.id), datetime(2026, 8, 16, 8, 0, tzinfo=UTC))
    import base64

    async with get_sessionmaker()() as db, db.begin():
        await apply_job_output(
            db,
            device.id,
            job_id,
            {
                "stream": "stdout",
                "data": base64.b64encode(b"nightly output\n").decode(),
                "entry_id": str(schedule.id),
            },
        )
        await apply_job_result(
            db, device.id, job_id, {"status": "succeeded", "entry_id": str(schedule.id)}
        )

    async with get_sessionmaker()() as db:
        run = await db.get(ScriptRun, job_id)
    assert run is not None
    assert "nightly output" in run.stdout


async def test_a_result_with_no_entry_id_is_still_refused():
    """Adoption must not become a way to invent runs. Only a result naming a
    schedule that exists gets a row."""
    device, _script, _schedule = await _fixture()
    async with get_sessionmaker()() as db, db.begin():
        await apply_job_result(db, device.id, uuid7(), {"status": "succeeded"})
        await apply_job_result(
            db, device.id, uuid7(), {"status": "succeeded", "entry_id": str(uuid7())}
        )

    async with get_sessionmaker()() as db:
        from sqlalchemy import func, select

        count = (await db.execute(select(func.count()).select_from(ScriptRun))).scalar_one()
    assert count == 0


async def test_reporting_the_same_fire_twice_updates_one_run():
    """The job id is a UUIDv5 over (entry, unjittered fire time) precisely so
    a redelivery is idempotent instead of a duplicate."""
    device, _script, schedule = await _fixture()
    job_id = scheduled_job_id(str(schedule.id), datetime(2026, 8, 16, 8, 0, tzinfo=UTC))

    for status in ("failed", "succeeded"):
        async with get_sessionmaker()() as db, db.begin():
            await apply_job_result(
                db, device.id, job_id, {"status": status, "entry_id": str(schedule.id)}
            )

    async with get_sessionmaker()() as db:
        from sqlalchemy import func, select

        count = (await db.execute(select(func.count()).select_from(ScriptRun))).scalar_one()
        run = await db.get(ScriptRun, job_id)
    assert count == 1
    assert run.status is RunStatus.succeeded


async def test_last_run_at_is_recorded_on_the_schedule():
    """So the schedules list can show whether a schedule is actually firing."""
    device, _script, schedule = await _fixture()
    job_id = scheduled_job_id(str(schedule.id), datetime(2026, 8, 16, 8, 0, tzinfo=UTC))

    async with get_sessionmaker()() as db, db.begin():
        await apply_job_result(
            db, device.id, job_id, {"status": "succeeded", "entry_id": str(schedule.id)}
        )

    async with get_sessionmaker()() as db:
        assert (await db.get(ScriptSchedule, schedule.id)).last_run_at is not None


def test_an_empty_schedule_is_version_zero():
    """What a fresh agent reports before it has been told anything.

    Any other value leaves every device in a fleet with no schedules
    permanently one version off, and the reconciler pushes an empty document
    to each of them: a NATS round trip per device to say nothing. Observed for
    real: agents reported 223132457, which is crc32("[]").
    """
    assert schedule_version([]) == 0


async def test_a_device_with_no_matching_schedules_needs_no_sync():
    device, _script, _schedule = await _fixture(target={"tags": ["servers"]}, tags=["laptops"])
    async with get_sessionmaker()() as db:
        doc = build_document(device, await load_schedules(db))
    assert doc["schedule_version"] == 0, "this device would be pushed an empty document for ever"
