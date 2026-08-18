"""Issuing, recording, renewing and revoking device certificates.

The CA itself (services/ca.py) is pure crypto and tested separately. This is the
half that has to answer operational questions: which devices are about to lose
network access, and what do I publish when one is stolen.

Expiry is the dangerous direction. For 802.1X an expired certificate locks a
device off the network, and a device that cannot reach the network cannot be
repaired remotely. Everything here exists so that is seen weeks early.
"""

import datetime as dt
import uuid

import pytest
from cryptography import x509
from cryptography.hazmat.primitives.asymmetric import ec

from openrmm.db.engine import session_scope
from openrmm.models.certificate import DeviceCertificate
from openrmm.models.device import Device, DeviceStatus, OsFamily
from openrmm.services.ca import CERT_LIFETIME, build_csr, init_ca
from openrmm.services.certificates import (
    CertificateRefusedError,
    build_crl,
    issue_for_device,
    needs_renewal,
    revoke_certificate,
)
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

PASSPHRASE = "test-passphrase"


@pytest.fixture
def ca(tmp_path):
    return init_ca(tmp_path, passphrase=PASSPHRASE, org="Test Org")


async def _device(status: DeviceStatus = DeviceStatus.active) -> uuid.UUID:
    async with session_scope() as db:
        d = Device(
            id=uuid7(), hostname="cert-host", os_family=OsFamily.linux, tags=[], status=status
        )
        db.add(d)
        await db.flush()
        return d.id


def _csr(common_name: str = "whatever-the-device-asked-for"):
    return build_csr(ec.generate_private_key(ec.SECP256R1()), common_name=common_name)


async def _issue(ca, device_id):
    async with session_scope() as db:
        return await issue_for_device(db, ca, device_id, _csr())


async def test_an_issued_certificate_is_stored_and_returned(ca):
    device_id = await _device()
    result = await _issue(ca, device_id)

    assert "BEGIN CERTIFICATE" in result.certificate_pem
    # The chain travels with it: a device that has only its own leaf cannot
    # present a path a verifier can build.
    assert result.chain_pem.count("BEGIN CERTIFICATE") >= 2

    async with session_scope() as db:
        row = await db.get(DeviceCertificate, result.serial)
    assert row is not None
    assert row.device_id == device_id
    assert row.not_after > row.not_before


async def test_the_common_name_is_the_device_id_not_what_the_csr_asked_for(ca):
    """The CSR is attacker-controlled input.

    A device asks for a key to be signed. It does not choose whose identity
    that key carries, or an enrolled machine could request a certificate naming
    a different one and authenticate as it.
    """
    device_id = await _device()
    result = await _issue(ca, device_id)
    cert = x509.load_pem_x509_certificate(result.certificate_pem.encode())
    cn = cert.subject.get_attributes_for_oid(x509.oid.NameOID.COMMON_NAME)[0].value
    assert cn == str(device_id)


async def test_a_retired_device_cannot_be_issued_a_certificate(ca):
    """Retirement deletes the agent credential. Handing out a network identity
    to a machine that was deliberately removed would put it back on the network
    by a different door."""
    device_id = await _device(DeviceStatus.retired)
    async with session_scope() as db:
        with pytest.raises(CertificateRefusedError):
            await issue_for_device(db, ca, device_id, _csr())


async def test_an_unknown_device_cannot_be_issued_a_certificate(ca):
    async with session_scope() as db:
        with pytest.raises(CertificateRefusedError):
            await issue_for_device(db, ca, uuid7(), _csr())


# --- renewal -----------------------------------------------------------------


async def test_a_fresh_certificate_does_not_need_renewal(ca):
    device_id = await _device()
    await _issue(ca, device_id)
    async with session_scope() as db:
        assert await needs_renewal(db, device_id) is False


async def test_a_device_with_no_certificate_needs_one(ca):
    device_id = await _device()
    async with session_scope() as db:
        assert await needs_renewal(db, device_id) is True


async def test_a_certificate_past_half_life_needs_renewal(ca):
    """Half life, not "nearly expired".

    The entire point of renewing early is to have weeks of failed attempts,
    alarms and human time before a device is locked off the network. Renewing
    at the end leaves no room for any of that.
    """
    device_id = await _device()
    result = await _issue(ca, device_id)

    async with session_scope() as db:
        row = await db.get(DeviceCertificate, result.serial)
        # Pretend it was issued most of a lifetime ago.
        aged = dt.datetime.now(dt.UTC) - (CERT_LIFETIME * 0.6)
        row.not_before = aged
        row.not_after = aged + CERT_LIFETIME

    async with session_scope() as db:
        assert await needs_renewal(db, device_id) is True


async def test_a_revoked_certificate_does_not_count_as_current(ca):
    device_id = await _device()
    result = await _issue(ca, device_id)
    async with session_scope() as db:
        await revoke_certificate(db, result.serial, reason="key-compromise")

    async with session_scope() as db:
        assert await needs_renewal(db, device_id) is True


# --- revocation and the CRL --------------------------------------------------


async def test_a_revoked_serial_appears_in_the_crl(ca):
    device_id = await _device()
    result = await _issue(ca, device_id)
    async with session_scope() as db:
        await revoke_certificate(db, result.serial, reason="key-compromise")

    async with session_scope() as db:
        crl_pem = await build_crl(db, ca)

    crl = x509.load_pem_x509_crl(crl_pem)
    revoked = {f"{r.serial_number:x}" for r in crl}
    assert result.serial in revoked


async def test_a_live_certificate_is_not_in_the_crl(ca):
    device_id = await _device()
    result = await _issue(ca, device_id)
    async with session_scope() as db:
        crl = x509.load_pem_x509_crl(await build_crl(db, ca))
    assert {f"{r.serial_number:x}" for r in crl} == set()
    assert result.serial


async def test_the_crl_is_signed_by_the_issuing_intermediate(ca):
    """A CRL the RADIUS server cannot verify is a CRL it ignores, and a CRL it
    ignores is a revocation that never happened."""
    async with session_scope() as db:
        crl = x509.load_pem_x509_crl(await build_crl(db, ca))
    assert crl.issuer == ca.intermediate_cert.subject
    assert crl.is_signature_valid(ca.intermediate_cert.public_key())


async def test_the_crl_carries_a_next_update(ca):
    """An expired CRL makes OpenSSL fail EVERY certificate, not just revoked
    ones. So the freshness window is both how fast a revocation takes effect
    and how long the network keeps working if publication breaks."""
    async with session_scope() as db:
        crl = x509.load_pem_x509_crl(await build_crl(db, ca))
    assert crl.next_update_utc is not None
    assert crl.next_update_utc > dt.datetime.now(dt.UTC)


async def test_revoking_twice_is_harmless(ca):
    device_id = await _device()
    result = await _issue(ca, device_id)
    async with session_scope() as db:
        await revoke_certificate(db, result.serial, reason="key-compromise")
    async with session_scope() as db:
        # An operator pressing the button again, or a retry after a timeout.
        await revoke_certificate(db, result.serial, reason="superseded")

    async with session_scope() as db:
        row = await db.get(DeviceCertificate, result.serial)
    assert row.revoked_at is not None
    assert row.revocation_reason == "key-compromise", (
        "the first revocation reason is the true one; a retry must not rewrite it"
    )
