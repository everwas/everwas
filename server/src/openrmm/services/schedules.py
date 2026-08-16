"""Scheduled-run job ids.

Schedules fire from the AGENT's local cache, including while it is offline, so
the server does not assign the job id for a scheduled run the way it does for
every other job. Both sides derive it instead, from the entry and the
unjittered fire time, and they must agree exactly: the id is what a result is
matched against, and a result whose id does not resolve to a row is logged as
"unknown run" and dropped.

This module is the Python half of that derivation. The Go half is
`JobID` in agent/internal/sched/sched.go, and each has a test asserting the
same vector.
"""

import uuid
from datetime import datetime

# uuid5(NAMESPACE_DNS, "schedule.openrmm.invalid"). Part of the wire contract:
# mirrored byte-for-byte by schedNamespace in agent/internal/sched/sched.go.
# Changing it orphans every scheduled result in flight.
SCHED_NAMESPACE = uuid.UUID("06cadeed-8a30-50ab-87f5-7a27b043ba2d")


def scheduled_job_id(entry_id: str, fire_at: datetime) -> uuid.UUID:
    """The job id an agent will report a scheduled run under.

    `fire_at` is the UNJITTERED scheduled time, which is what makes this
    idempotent: the agent spreads its actual start across the window, but two
    reports of the same nominal fire produce the same id, so a redelivery
    updates one run instead of creating a second.
    """
    return uuid.uuid5(SCHED_NAMESPACE, f"{entry_id}:{int(fire_at.timestamp())}")
