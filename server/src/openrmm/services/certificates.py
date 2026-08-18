"""Issuing, recording, renewing and revoking device certificates.

`services/ca.py` is the crypto: it signs, and refuses to sign things it should
not. This is the operational half, and it exists to answer two questions that a
sign-and-forget CA cannot:

  * Which devices are about to lose network access? For 802.1X an expired
    certificate locks a machine off the network, and a machine that cannot
    reach the network cannot be repaired remotely. That is a physical visit,
    per device, so certificates renew at half life and the weeks in between are
    for noticing.
  * What do I publish when one is stolen? Revocation for the network path is
    enforced by the RADIUS server reading a CRL, not by us refusing something.

The private key is never here. It is generated on the endpoint, ideally inside
a TPM, and only the CSR crosses the wire.
"""

from __future__ import annotations

import datetime as dt
import enum
import uuid
from dataclasses import dataclass

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from sqlalchemy import Select, select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.certificate import DeviceCertificate
from openrmm.models.device import Device, DeviceStatus
from openrmm.services.ca import CERT_LIFETIME, RENEW_AFTER, DeviceCa, issue_from_csr

#: How long a published CRL is considered fresh.
#:
#: Cuts both ways, which is why it is a deliberate constant rather than a
#: default. It bounds how long a revoked certificate keeps working, so shorter
#: is safer. It is ALSO the deadline on our own publication: OpenSSL fails
#: every certificate against an expired CRL, not just revoked ones, so a
#: publication outage longer than this takes the whole fleet off the network.
CRL_LIFETIME = dt.timedelta(hours=24)


class CertificateRefusedError(Exception):
    """This device will not be issued a certificate, and why."""


@dataclass
class IssuedCertificate:
    serial: str
    certificate_pem: str
    chain_pem: str
    not_after: dt.datetime


def _serial_hex(cert: x509.Certificate) -> str:
    return f"{cert.serial_number:x}"


async def issue_for_device(
    db: AsyncSession,
    ca: DeviceCa,
    device_id: uuid.UUID,
    csr: x509.CertificateSigningRequest,
    *,
    lifetime: dt.timedelta | None = None,
) -> IssuedCertificate:
    """Sign a device's CSR and record the result.

    The certificate's identity is the device id, taken from OUR record of which
    agent authenticated, never from the CSR. A CSR is attacker-controlled input:
    the device is asking for a key to be signed and does not get to choose whose
    identity that key then carries.
    """
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None:
        raise CertificateRefusedError("unknown device")
    if device.status is DeviceStatus.retired:
        # Retirement deletes the agent credential precisely to remove a machine
        # from the fleet. Handing it a network identity would put it back on
        # the network through a different door.
        raise CertificateRefusedError("device is retired")

    # The caller supplies the lifetime because it is deployment policy, not a
    # property of the CA: it depends on whether an expired certificate strands
    # the machine or merely drops it into remediation (ADR-0004).
    pem = issue_from_csr(
        ca, csr, common_name=str(device_id), lifetime=lifetime or CERT_LIFETIME
    )
    cert = x509.load_pem_x509_certificate(pem)

    db.add(
        DeviceCertificate(
            serial=_serial_hex(cert),
            device_id=device_id,
            common_name=str(device_id),
            certificate_pem=pem.decode(),
            fingerprint_sha256=cert.fingerprint(hashes.SHA256()).hex(),
            not_before=cert.not_valid_before_utc,
            not_after=cert.not_valid_after_utc,
        )
    )
    await db.flush()
    return IssuedCertificate(
        serial=_serial_hex(cert),
        certificate_pem=pem.decode(),
        # The chain travels with the leaf. A device holding only its own
        # certificate cannot present a path a verifier is able to build.
        chain_pem=ca.chain_pem.decode(),
        not_after=cert.not_valid_after_utc,
    )


async def current_certificate(db: AsyncSession, device_id: uuid.UUID) -> DeviceCertificate | None:
    """The live certificate for a device, or None.

    Newest first, and revoked ones are excluded: a revoked certificate is not a
    certificate this device has, it is one it must replace.
    """
    return (
        await db.execute(
            select(DeviceCertificate)
            .where(
                DeviceCertificate.device_id == device_id,
                DeviceCertificate.revoked_at.is_(None),
            )
            .order_by(DeviceCertificate.not_after.desc())
            .limit(1)
        )
    ).scalar_one_or_none()


async def needs_renewal(db: AsyncSession, device_id: uuid.UUID) -> bool:
    """Whether this device should ask for a new certificate now.

    True at HALF life, not near expiry. The gap is the whole safety margin: it
    is where failed attempts, alarms and someone noticing all have to fit
    before the device is locked off the network with no remote way back.
    """
    cert = await current_certificate(db, device_id)
    if cert is None:
        return True
    age = dt.datetime.now(dt.UTC) - cert.not_before
    return age >= RENEW_AFTER


async def expiring_soon(
    db: AsyncSession, within: dt.timedelta = CERT_LIFETIME / 2
) -> list[DeviceCertificate]:
    """Live certificates expiring inside `within`. The alarm's query."""
    cutoff = dt.datetime.now(dt.UTC) + within
    return list(
        (
            await db.execute(
                select(DeviceCertificate)
                .where(
                    DeviceCertificate.revoked_at.is_(None),
                    DeviceCertificate.not_after <= cutoff,
                )
                .order_by(DeviceCertificate.not_after)
            )
        )
        .scalars()
        .all()
    )


async def revoke_certificate(db: AsyncSession, serial: str, *, reason: str) -> None:
    """Mark a certificate revoked. Publication happens through the CRL.

    Revoking an already-revoked certificate is a no-op rather than an error:
    the operator pressing the button twice, or a client retrying after a
    timeout, must not be met with a failure. The FIRST reason is kept, because
    that is the one that was true.
    """
    cert = await db.get(DeviceCertificate, serial)
    if cert is None or cert.revoked_at is not None:
        return
    cert.revoked_at = dt.datetime.now(dt.UTC)
    cert.revocation_reason = reason
    await db.flush()


async def build_crl(db: AsyncSession, ca: DeviceCa) -> bytes:
    """Build and sign a CRL over every revoked certificate. Returns PEM.

    Signed by the issuing intermediate, because a CRL the RADIUS server cannot
    verify is a CRL it ignores, and a revocation it ignores never happened.

    Expired certificates are still listed. Dropping them once they expire is
    the obvious optimisation and it is wrong while any verifier has a clock
    that disagrees with ours: a certificate we consider expired is one they may
    still accept, and the CRL entry is the only thing that stops them.
    """
    now = dt.datetime.now(dt.UTC)
    revoked = (
        (
            await db.execute(
                select(DeviceCertificate).where(DeviceCertificate.revoked_at.is_not(None))
            )
        )
        .scalars()
        .all()
    )

    builder = (
        x509.CertificateRevocationListBuilder()
        .issuer_name(ca.intermediate_cert.subject)
        .last_update(now - dt.timedelta(minutes=5))
        .next_update(now + CRL_LIFETIME)
    )
    for cert in revoked:
        builder = builder.add_revoked_certificate(
            x509.RevokedCertificateBuilder()
            .serial_number(int(cert.serial, 16))
            .revocation_date(cert.revoked_at)
            .build()
        )
    crl = builder.sign(private_key=ca.intermediate_key, algorithm=hashes.SHA256())
    return crl.public_bytes(serialization.Encoding.PEM)


class DriftKind(enum.StrEnum):
    """Why a device's reported certificate does not match our records."""

    #: The device reports a serial we have no record of issuing. Most often a
    #: machine cloned from an image that carried somebody else's certificate,
    #: or one enrolled against a different server.
    unknown = "unknown"
    #: The device reports a certificate we issued, but not its newest one.
    #: Either a renewal was issued and never saved, or the machine was restored
    #: from a backup taken before the renewal.
    stale = "stale"
    #: The device says it holds nothing, though we have issued it something.
    #: Material deleted by hand, or a failed write that left nothing behind.
    missing = "missing"


@dataclass
class CertificateDrift:
    device_id: uuid.UUID
    hostname: str
    kind: DriftKind
    reported_serial: str | None
    issued_serial: str | None
    reported_at: dt.datetime | None


async def certificate_drift(
    db: AsyncSession, query: Select | None = None
) -> list[CertificateDrift]:
    """Devices holding something other than what we last issued them.

    This is the whole reason the heartbeat carries a serial. Knowing what we
    issued answers a different question from knowing what the machine has, and
    the gap between them is invisible until it surfaces as an authentication
    failure nobody can account for.

    Devices that have never reported are skipped rather than flagged. An agent
    too old to say so is not evidence of drift, and a fleet mid-upgrade would
    otherwise light up entirely with a finding that means "your agents are
    old", which is true, unrelated, and would bury the real ones.

    `query` is the caller's already-scoped device selection, so the tenant
    boundary is applied by the caller in the one way this codebase enforces it
    rather than re-derived here.
    """
    if query is None:
        query = select(Device)
    devices = list((await db.execute(query)).scalars().all())

    out: list[CertificateDrift] = []
    for device in devices:
        if device.reported_cert_at is None:
            continue  # never told us; not the same as telling us it has nothing

        issued = await current_certificate(db, device.id)
        issued_serial = issued.serial if issued else None
        reported = device.reported_cert_serial

        if not reported:
            if issued_serial is None:
                continue  # holds nothing, was issued nothing: consistent
            kind = DriftKind.missing
        elif reported == issued_serial:
            continue  # the ordinary, healthy case
        else:
            known = (
                await db.execute(
                    select(DeviceCertificate.serial).where(
                        DeviceCertificate.serial == reported,
                        DeviceCertificate.device_id == device.id,
                    )
                )
            ).scalar_one_or_none()
            kind = DriftKind.stale if known else DriftKind.unknown

        out.append(
            CertificateDrift(
                device_id=device.id,
                hostname=device.hostname,
                kind=kind,
                reported_serial=reported or None,
                issued_serial=issued_serial,
                reported_at=device.reported_cert_at,
            )
        )
    return out
