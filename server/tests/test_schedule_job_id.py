"""The scheduled job id must be byte-identical on both sides.

The agent computes it in Go, the server in Python, and neither ever sees the
other's value until a result arrives. If they drift, the result is dropped as
"unknown run": no error, no alert, just a nightly job that silently stops
producing records. The shared vector below is the only thing that catches it.

Its twin is TestJobIDMatchesTheServersDerivation in
agent/internal/sched/sched_test.go.
"""

import uuid
from datetime import UTC, datetime

from openrmm.services.schedules import SCHED_NAMESPACE, scheduled_job_id

# Frozen: agent and server both assert this exact string.
VECTOR_ENTRY = "nightly-scan"
VECTOR_FIRE = datetime.fromtimestamp(1755225600, tz=UTC)
VECTOR_ID = "11bea7a9-2e3c-5b06-90a7-0e4c7ba7c2f1"


def test_matches_the_agents_derivation():
    assert str(scheduled_job_id(VECTOR_ENTRY, VECTOR_FIRE)) == VECTOR_ID


def test_namespace_is_frozen():
    """The namespace is wire contract, not a constant to tidy up. Changing it
    orphans every scheduled result in flight."""
    assert str(SCHED_NAMESPACE) == "06cadeed-8a30-50ab-87f5-7a27b043ba2d"


def test_is_a_uuid5():
    """Results ingest parses the job id as a UUID. A scheduled id that is not
    one never resolves to a run."""
    got = scheduled_job_id(VECTOR_ENTRY, VECTOR_FIRE)
    assert isinstance(got, uuid.UUID)
    assert got.version == 5


def test_same_fire_is_the_same_id():
    """What makes a doubly-reported run idempotent instead of a duplicate."""
    assert scheduled_job_id("nightly", VECTOR_FIRE) == scheduled_job_id("nightly", VECTOR_FIRE)


def test_different_fires_and_entries_differ():
    later = datetime.fromtimestamp(1755225600 + 3600, tz=UTC)
    assert scheduled_job_id("nightly", VECTOR_FIRE) != scheduled_job_id("nightly", later)
    assert scheduled_job_id("nightly", VECTOR_FIRE) != scheduled_job_id("weekly", VECTOR_FIRE)


def test_timezone_does_not_change_the_id():
    """The agent sends a unix timestamp. Two datetimes for the same instant in
    different zones must not produce different ids, or an agent in Denver and
    a server in UTC disagree about every scheduled run."""
    from datetime import timedelta, timezone

    mst = VECTOR_FIRE.astimezone(timezone(timedelta(hours=-7)))
    assert scheduled_job_id("nightly", mst) == scheduled_job_id("nightly", VECTOR_FIRE)
