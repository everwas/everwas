import uuid
from datetime import datetime
from pathlib import Path

import structlog
from cryptography import x509
from fastapi import APIRouter, HTTPException, status
from pydantic import BaseModel, Field

from openrmm.api.deps import DbSession
from openrmm.config import get_settings
from openrmm.schemas.enrollment import EnrollRequest, EnrollResponse
from openrmm.services.ca import CaNotInitialisedError, CsrRejectedError, load_ca
from openrmm.services.certificates import CertificateRefusedError, issue_for_device
from openrmm.services.enrollment import (
    EnrollmentError,
    RevokedCredentialError,
    UnknownCredentialError,
    enroll_device,
    renew_agent_secret,
    verify_agent_secret,
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


class CertificateRequest(BaseModel):
    """An agent asking for its network certificate.

    Authenticated by the credential it already holds, exactly as renewal is. It
    cannot be a session (the caller is a machine) and it cannot be the
    certificate (this is how a machine with none gets one).
    """

    agent_id: uuid.UUID
    agent_secret: str = Field(min_length=8)
    #: The CSR. The private key stays on the endpoint and never crosses the
    #: wire: a key that travelled is a key that was in a log, a proxy buffer
    #: and somebody's database.
    csr_pem: str = Field(min_length=1)


class CertificateResponse(BaseModel):
    certificate_pem: str
    chain_pem: str
    serial: str
    not_after: datetime


@router.post("/certificate")
async def issue_certificate(body: CertificateRequest, db: DbSession) -> CertificateResponse:
    """Sign an agent's CSR into a device certificate.

    The identity on the certificate is the device id we resolved from the
    credential, never the subject in the CSR. A CSR is attacker-controlled
    input: the device is asking for a key to be signed, not choosing whose
    identity that key carries.
    """
    settings = get_settings()
    if not settings.ca_passphrase:
        # Off by default. A CA that materialises with a passphrase nobody chose
        # is a CA nobody is guarding.
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "certificate issuance is not configured on this server",
        )

    if not await verify_agent_secret(db, body.agent_id, body.agent_secret):
        log.warning("certificate request refused", agent_id=str(body.agent_id))
        raise HTTPException(status.HTTP_403_FORBIDDEN, "certificate request refused")

    try:
        csr = x509.load_pem_x509_csr(body.csr_pem.encode())
    except ValueError as exc:
        # The caller's error, not a crash. A malformed CSR from a broken agent
        # build must not read as a server fault.
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "malformed CSR") from exc

    try:
        ca = load_ca(Path(settings.ca_dir), passphrase=settings.ca_passphrase)
        issued = await issue_for_device(db, ca, body.agent_id, csr)
    except CaNotInitialisedError as exc:
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE, "no certificate authority on this server"
        ) from exc
    except CsrRejectedError as exc:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, str(exc)) from exc
    except CertificateRefusedError as exc:
        raise HTTPException(status.HTTP_403_FORBIDDEN, str(exc)) from exc

    await db.commit()
    log.info(
        "device certificate issued",
        agent_id=str(body.agent_id),
        serial=issued.serial,
        not_after=issued.not_after.isoformat(),
    )
    return CertificateResponse(
        certificate_pem=issued.certificate_pem,
        chain_pem=issued.chain_pem,
        serial=issued.serial,
        not_after=issued.not_after,
    )
