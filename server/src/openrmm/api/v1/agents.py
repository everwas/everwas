from fastapi import APIRouter, HTTPException, status

from openrmm.api.deps import DbSession
from openrmm.config import get_settings
from openrmm.schemas.enrollment import EnrollRequest, EnrollResponse
from openrmm.services.enrollment import EnrollmentError, enroll_device

router = APIRouter()


@router.post("/enroll", status_code=status.HTTP_201_CREATED)
async def enroll(body: EnrollRequest, db: DbSession) -> EnrollResponse:
    try:
        device, agent_secret = await enroll_device(db, body)
    except EnrollmentError as exc:
        # One generic message: don't leak whether a token exists vs is spent.
        raise HTTPException(status.HTTP_403_FORBIDDEN, "enrollment refused") from exc
    await db.commit()
    return EnrollResponse(
        agent_id=device.id,
        agent_secret=agent_secret,
        nats_url=get_settings().nats_public_url,
    )
