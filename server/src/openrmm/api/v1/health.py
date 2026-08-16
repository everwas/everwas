from datetime import UTC, datetime, timedelta

from fastapi import APIRouter, Response, status
from sqlalchemy import func, select, text

from openrmm import __version__
from openrmm.api.deps import DbSession
from openrmm.models.deadletter import IngestDeadLetter
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.telemetry import DeviceStatusLatest

router = APIRouter()

# If devices are reporting, the newest sample should never be older than this.
INGEST_STALE_AFTER = timedelta(minutes=10)
DEAD_LETTER_WINDOW = timedelta(hours=24)


@router.get("/health")
async def health(db: DbSession) -> dict:
    """Liveness only: is this process up and can it reach the database."""
    await db.execute(text("SELECT 1"))
    return {"status": "ok", "version": __version__}


@router.get("/health/ingest")
async def ingest_health(db: DbSession, response: Response) -> dict:
    """Is data actually still arriving.

    `/health` returning ok told an operator nothing: every silent-failure mode
    in this system leaves the API perfectly healthy while ingest, alerting, or
    delivery has stopped. This endpoint answers the question that matters, and
    the rule is that SILENCE IS THE ALARM. If devices are online but no sample
    has landed recently, that is a fault, not a quiet period.
    """
    now = datetime.now(UTC)

    active = (
        await db.execute(
            select(func.count()).select_from(Device).where(Device.status == DeviceStatus.active)
        )
    ).scalar_one()
    newest = (await db.execute(select(func.max(DeviceStatusLatest.ts)))).scalar_one_or_none()
    dead_lettered = (
        await db.execute(
            select(func.count())
            .select_from(IngestDeadLetter)
            .where(IngestDeadLetter.at > now - DEAD_LETTER_WINDOW)
        )
    ).scalar_one()

    stale_for = (now - newest).total_seconds() if newest else None
    problems: list[str] = []
    # Only assert staleness when something SHOULD be reporting. An empty fleet
    # is quiet for a good reason.
    if active and (newest is None or now - newest > INGEST_STALE_AFTER):
        problems.append(
            f"{active} device(s) active but newest telemetry is "
            + (f"{stale_for:.0f}s old" if stale_for else "absent")
        )
    if dead_lettered:
        problems.append(f"{dead_lettered} message(s) dead-lettered in the last 24h")

    if problems:
        response.status_code = status.HTTP_503_SERVICE_UNAVAILABLE

    return {
        "status": "degraded" if problems else "ok",
        "problems": problems,
        "active_devices": active,
        "newest_telemetry_age_s": round(stale_for) if stale_for is not None else None,
        "dead_lettered_24h": dead_lettered,
    }
