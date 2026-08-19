"""The device-issuing certificate authority.

Issues CLIENT_AUTH certificates for endpoints to use with EAP-TLS. Distinct
from l2trace's own CA, which issues the SERVER_AUTH leaf the RADIUS server
presents: keeping the chains separate means that compromising the device issuer
lets an attacker mint a device identity, but NOT stand up a rogue access point
that supplicants trust and harvest credentials from. See ADR-0003.

The properties worth pinning are mostly about what the CA refuses to do.
"""

import datetime as dt

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

from everwas.services.ca import (
    CERT_LIFETIME,
    CaNotInitialisedError,
    CsrRejectedError,
    build_csr,
    init_ca,
    issue_from_csr,
    load_ca,
)

PASSPHRASE = "test-passphrase-not-a-real-one"


@pytest.fixture
def ca(tmp_path):
    """A freshly initialised CA, on disk, unlocked with a passphrase."""
    return init_ca(tmp_path, passphrase=PASSPHRASE, org="Test Org")


def _leaf(ca, *, common_name="device-1") -> x509.Certificate:
    key = ec.generate_private_key(ec.SECP256R1())
    csr = build_csr(key, common_name=common_name)
    return x509.load_pem_x509_certificate(issue_from_csr(ca, csr, common_name=common_name))


# --- key custody -------------------------------------------------------------


def test_the_root_private_key_is_not_persisted(ca, tmp_path):
    """It is handed to the operator once and never written.

    A root key sitting on the server is a root key that leaks with the server.
    Day-to-day issuance uses the intermediate, so the root is only needed again
    to mint a replacement intermediate, which is a deliberate, rare, offline
    act.
    """
    assert ca.root_key_pem, "init must return the root key for the operator to store"

    on_disk = b"".join(p.read_bytes() for p in tmp_path.rglob("*") if p.is_file())
    assert ca.root_key_pem not in on_disk, "the root private key was written to disk"


def test_the_intermediate_key_is_encrypted_at_rest(ca, tmp_path):
    """A database or volume compromise must not yield a usable signing key."""
    stored = b"".join(p.read_bytes() for p in tmp_path.rglob("*") if p.is_file())
    assert b"ENCRYPTED" in stored or b"BEGIN ENCRYPTED PRIVATE KEY" in stored

    # ValueError is what cryptography raises for a bad passphrase. Named
    # rather than blanket-caught, so this cannot start passing because of an
    # ImportError or a typo in the call.
    with pytest.raises(ValueError):
        load_ca(tmp_path, passphrase="wrong-passphrase")


def test_reloading_with_the_right_passphrase_works(ca, tmp_path):
    reloaded = load_ca(tmp_path, passphrase=PASSPHRASE)
    assert reloaded.intermediate_cert.subject == ca.intermediate_cert.subject


def test_an_uninitialised_directory_says_so(tmp_path):
    with pytest.raises(CaNotInitialisedError):
        load_ca(tmp_path / "nothing-here", passphrase=PASSPHRASE)


# --- what it issues ----------------------------------------------------------


def test_an_issued_certificate_is_for_client_auth_only(ca):
    """SERVER_AUTH here would let a device impersonate the RADIUS server.

    That is the rogue-AP attack: a supplicant that trusts this chain would
    complete a handshake against the attacker and hand over whatever the inner
    method carries. The EKU is the thing standing in the way.
    """
    cert = _leaf(ca)
    eku = cert.extensions.get_extension_for_class(x509.ExtendedKeyUsage).value
    assert ExtendedKeyUsageOID.CLIENT_AUTH in eku
    assert ExtendedKeyUsageOID.SERVER_AUTH not in eku


def test_an_issued_certificate_cannot_sign_others(ca):
    cert = _leaf(ca)
    bc = cert.extensions.get_extension_for_class(x509.BasicConstraints).value
    assert bc.ca is False, "a leaf that can sign is a second certificate authority"


def test_the_leaf_chains_to_the_intermediate_not_the_root(ca):
    cert = _leaf(ca)
    assert cert.issuer == ca.intermediate_cert.subject


def test_the_lifetime_is_short_enough_to_bound_a_stolen_key(ca):
    cert = _leaf(ca)
    life = cert.not_valid_after_utc - cert.not_valid_before_utc
    assert life <= CERT_LIFETIME + dt.timedelta(days=1)
    # And long enough to survive a holiday, since renewal happens at half life.
    assert life >= dt.timedelta(days=30)


def test_the_identity_comes_from_the_server_not_the_csr(ca):
    """A CSR is attacker-controlled input.

    The device asks for a key to be signed; it does not get to choose whose
    identity that key carries. Taking the subject from the CSR would let an
    enrolled device request a certificate naming a different device, or an
    administrator.
    """
    key = ec.generate_private_key(ec.SECP256R1())
    lying_csr = build_csr(key, common_name="somebody-else")

    cert = x509.load_pem_x509_certificate(
        issue_from_csr(ca, lying_csr, common_name="the-real-device")
    )
    cn = cert.subject.get_attributes_for_oid(NameOID.COMMON_NAME)[0].value
    assert cn == "the-real-device"


def test_each_certificate_has_a_distinct_serial(ca):
    serials = {_leaf(ca, common_name=f"d{i}").serial_number for i in range(5)}
    assert len(serials) == 5, "a reused serial makes revocation ambiguous"


# --- what it refuses ---------------------------------------------------------


def test_a_csr_with_a_bad_signature_is_refused(ca):
    """Proof of possession. Without it, anyone can have a certificate issued
    for a public key whose private half belongs to someone else."""
    key = ec.generate_private_key(ec.SECP256R1())
    good = build_csr(key, common_name="device-1")

    # Corrupt the signature while keeping the structure parseable.
    raw = bytearray(good.public_bytes(serialization.Encoding.DER))
    raw[-1] ^= 0xFF
    try:
        tampered = x509.load_der_x509_csr(bytes(raw))
    except ValueError:
        pytest.skip("mutation produced an unparseable CSR; signature check is upstream")

    with pytest.raises(CsrRejectedError):
        issue_from_csr(ca, tampered, common_name="device-1")


def test_a_weak_key_is_refused(ca):
    """1024-bit RSA is signable and worthless. Refusing at issuance is the only
    point where we can still say no."""
    from cryptography.hazmat.primitives.asymmetric import rsa

    weak = rsa.generate_private_key(public_exponent=65537, key_size=1024)
    csr = build_csr(weak, common_name="weak-device")
    with pytest.raises(CsrRejectedError):
        issue_from_csr(ca, csr, common_name="weak-device")


def test_the_signature_algorithm_is_not_sha1(ca):
    cert = _leaf(ca)
    assert not isinstance(cert.signature_hash_algorithm, hashes.SHA1)
