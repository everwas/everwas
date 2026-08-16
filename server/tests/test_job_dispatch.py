"""Job dispatch must never run work the database has no record of.

The defect these tests pin down: run rows were flushed (uncommitted), each job
was published to JetStream in a loop, and the caller committed afterwards. A
publish failure part-way through a fleet run rolled every row back while the
already-published jobs stayed in the stream for agents to fetch and execute as
root. The operator saw HTTP 500 and reasonably concluded nothing had run.

The invariant asserted throughout is one sentence: every job the fleet was
told about has a committed row that describes it.
"""

import contextlib
import uuid

import pytest
from sqlalchemy import select, update

from openrmm.db.engine import get_sessionmaker
from openrmm.models.audit import AuditLog
from openrmm.models.device import Device, OsFamily
from openrmm.models.job_outbox import JobOutbox, JobOutboxStatus
from openrmm.models.patch import PatchJob, PatchJobStatus
from openrmm.models.script import RunStatus, Script, ScriptRun, ShellKind
from openrmm.services import job_outbox as drainer
from openrmm.services.jobs import (
    AmbiguousTarget,
    TooManyTargets,
    queue_script_run,
    resolve_targets,
)
from openrmm.services.patching import queue_patch_install
from openrmm.util.ids import uuid7

FLEET = 5


class FakeJetStream:
    """Records what the fleet was actually told, and can fail on cue."""

    def __init__(self, fail_from: int | None = None):
        self.fail_from = fail_from
        self.published: list[tuple[str, bytes, dict]] = []

    async def publish(self, subject, payload=b"", headers=None, **_kw):
        if self.fail_from is not None and len(self.published) >= self.fail_from:
            raise ConnectionError("nats: no responders available for request")
        self.published.append((subject, payload, headers or {}))

    @property
    def job_ids(self) -> set[str]:
        return {h["Nats-Msg-Id"] for _, _, h in self.published}


class FakeNats:
    def __init__(self, js: FakeJetStream):
        self._js = js

    def jetstream(self) -> FakeJetStream:
        return self._js


async def _fleet(count: int = FLEET) -> tuple[Script, list[Device]]:
    async with get_sessionmaker()() as db, db.begin():
        script = Script(
            id=uuid.uuid4(),
            name="reboot-everything",
            shell=ShellKind.bash,
            body="#!/bin/bash\nrm -rf /nothing\n",
            sha256="0" * 64,
        )
        db.add(script)
        devices = [
            Device(id=uuid7(), hostname=f"host-{i:02d}", os_family=OsFamily.linux, tags=["prod"])
            for i in range(count)
        ]
        for device in devices:
            db.add(device)
    return script, devices


async def _committed_run_ids() -> set[str]:
    async with get_sessionmaker()() as db:
        return {str(r) for r in (await db.execute(select(ScriptRun.id))).scalars()}


async def _outbox_rows() -> list[JobOutbox]:
    async with get_sessionmaker()() as db:
        return list((await db.execute(select(JobOutbox).order_by(JobOutbox.id))).scalars())


async def _make_all_due() -> None:
    """Pretend the backoff elapsed, so a test does not have to sleep for it."""
    async with get_sessionmaker()() as db, db.begin():
        await db.execute(
            update(JobOutbox)
            .where(JobOutbox.status == JobOutboxStatus.pending)
            .values(next_attempt_at=JobOutbox.created_at)
        )


# --- the regression -------------------------------------------------------


async def test_queueing_publishes_nothing_and_commits_everything(pg_database):
    """Queue a fleet-wide run against a broker that fails on the third publish.

    Before the fix: two jobs are in the stream, the exception rolls the
    transaction back, and the database has no run rows at all. Two machines
    execute a script nobody can see, and `published <= committed` fails.
    """
    script, devices = await _fleet()
    js = FakeJetStream(fail_from=2)
    nc = FakeNats(js)

    # Exactly what the API does: on an exception the session rolls back, and
    # the operator is told the run failed.
    with contextlib.suppress(ConnectionError):
        async with get_sessionmaker()() as db, db.begin():
            await queue_script_run(db, nc, script, devices, requested_by="operator@example.com")

    committed = await _committed_run_ids()
    assert js.job_ids <= committed, (
        f"jobs were published that no committed run row describes: {sorted(js.job_ids - committed)}"
    )
    assert not js.published, "queueing must not publish inline; the dispatcher delivers"

    # ...and the other half of "everything or nothing": everything is recorded.
    assert len(committed) == FLEET
    rows = await _outbox_rows()
    assert {str(r.id) for r in rows} == committed
    assert all(r.status is JobOutboxStatus.pending for r in rows)
    assert all(r.subject == f"jobs.{r.device_id}" for r in rows)


async def test_nothing_is_recorded_when_the_transaction_fails(pg_database):
    """The mirror case: the caller's commit never happens, so nothing exists."""
    script, devices = await _fleet()
    js = FakeJetStream()

    with pytest.raises(RuntimeError):
        async with get_sessionmaker()() as db, db.begin():
            await queue_script_run(
                db, FakeNats(js), script, devices, requested_by="operator@example.com"
            )
            raise RuntimeError("commit fails here")

    assert await _committed_run_ids() == set()
    assert await _outbox_rows() == []
    assert not js.published, "a rolled-back run must leave nothing for an agent to fetch"


# --- delivery -------------------------------------------------------------


async def test_drainer_delivers_each_job_once(pg_database):
    script, devices = await _fleet()
    async with get_sessionmaker()() as db, db.begin():
        _, runs = await queue_script_run(db, None, script, devices, requested_by="op@example.com")
    run_ids = {str(r.id) for r in runs}

    js = FakeJetStream()
    assert await drainer.drain_job_outbox(js) == FLEET
    assert js.job_ids == run_ids
    assert {s for s, _, _ in js.published} == {f"jobs.{d.id}" for d in devices}

    rows = await _outbox_rows()
    assert all(r.status is JobOutboxStatus.published for r in rows)
    assert all(r.published_at is not None for r in rows)

    # A second pass has nothing to do, so no agent sees the job twice.
    assert await drainer.drain_job_outbox(js) == 0
    assert len(js.published) == FLEET


async def test_mid_batch_publish_failure_strands_nothing(pg_database):
    """A broker that dies half way through must leave the rest deliverable."""
    script, devices = await _fleet()
    async with get_sessionmaker()() as db, db.begin():
        _, runs = await queue_script_run(db, None, script, devices, requested_by="op@example.com")
    run_ids = {str(r.id) for r in runs}

    failing = FakeJetStream(fail_from=2)
    await drainer.drain_job_outbox(failing)

    assert failing.job_ids <= await _committed_run_ids()
    rows = await _outbox_rows()
    assert sum(r.status is JobOutboxStatus.published for r in rows) == 2
    pending = [r for r in rows if r.status is JobOutboxStatus.pending]
    assert len(pending) == FLEET - 2
    # Exactly one row paid for the failure; the rest were left untouched.
    assert sorted(r.attempts for r in pending) == [0] * (FLEET - 3) + [1]
    assert any("no responders" in (r.last_error or "") for r in pending)

    # Every run is still visible as queued, and the broker coming back finishes
    # the job. No operator intervention, no duplicate delivery.
    async with get_sessionmaker()() as db:
        statuses = {r.status for r in (await db.execute(select(ScriptRun))).scalars()}
    assert statuses == {RunStatus.queued}

    await _make_all_due()
    healthy = FakeJetStream()
    await drainer.drain_job_outbox(healthy)
    assert healthy.job_ids | failing.job_ids == run_ids
    assert not (healthy.job_ids & failing.job_ids), "a job was delivered twice"


async def test_undeliverable_run_is_marked_failed_not_queued_forever(pg_database):
    """After the last attempt the run says why it will never happen."""
    script, devices = await _fleet(1)
    async with get_sessionmaker()() as db, db.begin():
        _, runs = await queue_script_run(db, None, script, devices, requested_by="op@example.com")
    run_id = runs[0].id

    js = FakeJetStream(fail_from=0)
    for _ in range(drainer.MAX_ATTEMPTS):
        await _make_all_due()
        await drainer.drain_job_outbox(js)

    row = (await _outbox_rows())[0]
    assert row.status is JobOutboxStatus.failed
    assert row.attempts == drainer.MAX_ATTEMPTS

    async with get_sessionmaker()() as db:
        run = (await db.execute(select(ScriptRun).where(ScriptRun.id == run_id))).scalar_one()
        assert run.status is RunStatus.failed, "a run that will never be sent must not read queued"
        assert "never dispatched" in run.stderr
        assert run.finished_at is not None
        actions = list((await db.execute(select(AuditLog.action))).scalars())
    assert "job.dispatch_failed" in actions


async def test_undeliverable_patch_job_is_marked_failed(pg_database):
    _, devices = await _fleet(1)
    async with get_sessionmaker()() as db, db.begin():
        job = await queue_patch_install(
            db, devices[0], ["KB5000001"], requested_by="op@example.com"
        )
        job_id = job.id

    js = FakeJetStream(fail_from=0)
    for _ in range(drainer.MAX_ATTEMPTS):
        await _make_all_due()
        await drainer.drain_job_outbox(js)

    assert not js.published
    async with get_sessionmaker()() as db:
        patch_job = (await db.execute(select(PatchJob).where(PatchJob.id == job_id))).scalar_one()
        assert patch_job.status is PatchJobStatus.failed
        assert "never dispatched" in patch_job.log
        assert patch_job.failed == {"KB5000001": "not dispatched"}


# --- targeting ------------------------------------------------------------


async def test_ambiguous_target_is_a_hard_error():
    """{device_ids: [...], all: true} used to silently mean device_ids."""
    with pytest.raises(AmbiguousTarget) as exc:
        await resolve_targets(None, {"device_ids": [str(uuid.uuid4())], "all": True})
    assert "device_ids" in str(exc.value) and "all" in str(exc.value)

    with pytest.raises(AmbiguousTarget):
        await resolve_targets(None, {"tags": ["prod"], "all": True})


async def test_empty_selector_still_means_nothing():
    """What the API sends when the operator picked no target at all."""
    assert await resolve_targets(None, {"device_ids": [], "tags": [], "all": False}) == []


async def test_fleet_wide_run_is_capped(pg_database):
    _, devices = await _fleet()
    async with get_sessionmaker()() as db:
        with pytest.raises(TooManyTargets) as exc:
            await resolve_targets(db, {"all": True}, max_targets=FLEET - 1)
        assert str(FLEET) in str(exc.value), "the error must name the count it refused"
        assert len(await resolve_targets(db, {"all": True}, max_targets=FLEET)) == FLEET
        assert len(devices) == FLEET
