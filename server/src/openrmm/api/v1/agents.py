import uuid

import structlog
from fastapi import APIRouter, HTTPException, status
from pydantic import BaseModel, Field

from openrmm.api.deps import DbSession
from openrmm.config import get_settings
from openrmm.schemas.enrollment import EnrollRequest, EnrollResponse
from openrmm.services.enrollment import (
    EnrollmentError,
    RevokedCredentialError,
    UnknownCredentialError,
    enroll_device,
    renew_agent_secret,
)

router = APIRouter()
log = structlog.get_logger()


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


class RenewRequest(BaseModel):
    """An agent asking for its own replacement credential.

    agent_id plus the secret it currently holds. No session, no API key: the
    credential IS the authentication, exactly as the one-time token is for
    enrollment.
    """

    agent_id: uuid.UUID
    agent_secret: str = Field(min_length=8)


class RenewResponse(BaseModel):
    agent_secret: str


@router.post("/renew")
async def renew(body: RenewRequest, db: DbSession) -> RenewResponse:
    """Exchange the credential an agent holds for a fresh one.

    PULL, not push, and that is the whole point. Rotation used to be delivered
    over NATS to a machine that might be switched off, with a 24 hour deadline
    on the old secret and nothing retrying: a laptop away for a long weekend
    booted holding a secret that had already expired, and the recovery was a
    site visit per host. An agent that asks cannot miss the delivery.

    Public, like /enroll, because an agent that needs a new credential by
    definition cannot present a valid session. The presented secret is what
    authenticates it, and a wrong one is refused with the same generic message
    for the same reason: do not leak whether a device exists.
    """
    try:
        secret = await renew_agent_secret(db, body.agent_id, body.agent_secret)
    except (UnknownCredentialError, RevokedCredentialError) as exc:
        log.warning("agent renewal refused", agent_id=str(body.agent_id), reason=str(exc))
        raise HTTPException(status.HTTP_403_FORBIDDEN, "renewal refused") from exc
    await db.commit()
    log.info("agent credential renewed", agent_id=str(body.agent_id))
    return RenewResponse(agent_secret=secret)
