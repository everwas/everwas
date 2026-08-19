"""The device-issuing certificate authority.

Issues CLIENT_AUTH certificates that endpoints present for EAP-TLS network
authentication (ADR-0003). It is deliberately NOT the same authority as the one
issuing l2trace's RADIUS server certificate: compromising this one lets an
attacker mint a device identity, which is bad, but if it also signed the server
leaf they could additionally stand up a rogue access point that every supplicant
trusts, and harvest from the whole estate. Two chains, one blast radius each.

Key custody, which is the decision everything else rests on:

  * `init_ca` mints a root and an intermediate. The root private key is
    RETURNED to the caller and never written. The operator stores it offline;
    it is only needed again to mint a replacement intermediate, which is a rare
    and deliberate act.
  * The intermediate private key is written encrypted, with a passphrase from
    the environment. Day-to-day signing uses it, so a stolen volume or database
    dump yields neither a usable signing key nor the root.

This is the shape an operator with no HSM can actually run. The upgrade path is
to keep the intermediate in a KMS or HSM instead, which changes `load_ca` and
nothing else.
"""

from __future__ import annotations

import datetime as dt
import secrets
from dataclasses import dataclass
from pathlib import Path

from cryptography import x509
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec, rsa
from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

#: How long an issued device certificate lives.
#:
#: A compromise between two failures. Too long and a stolen key that could not
#: be bound to a TPM stays useful for months. Too short and a device that is
#: switched off for a holiday comes back to a certificate that expired while it
#: slept, which for 802.1X means it cannot reach the network to be fixed: a
#: physical visit, per device. Ninety days with renewal at half life leaves
#: forty-five days of retries before anything is at risk.
CERT_LIFETIME = dt.timedelta(days=90)

#: Renew at half life. Not "shortly before expiry": the entire point is to have
#: weeks of failed attempts, alarms and human time before a device is locked
#: off the network.
RENEW_AFTER = CERT_LIFETIME / 2

#: The CA certificates themselves outlive many generations of leaves. Rotating
#: a CA means re-issuing to the whole fleet, so this is deliberately long.
CA_LIFETIME = dt.timedelta(days=3650)
INTERMEDIATE_LIFETIME = dt.timedelta(days=1825)

#: Refuse a key too weak to be worth signing. Issuance is the last point where
#: we can still say no; afterwards the certificate is somebody's network
#: identity for ninety days.
MIN_RSA_BITS = 2048

_ROOT_CERT = "root.crt"
_INTERMEDIATE_CERT = "intermediate.crt"
_INTERMEDIATE_KEY = "intermediate.key"
_CHAIN = "chain.pem"


class CaNotInitialisedError(Exception):
    """No CA material where one was expected."""


class CsrRejectedError(Exception):
    """The certificate signing request will not be signed, and why."""


@dataclass
class DeviceCa:
    """Loaded CA material. Holds the intermediate signing key in memory."""

    intermediate_cert: x509.Certificate
    intermediate_key: ec.EllipticCurvePrivateKey
    root_cert: x509.Certificate
    #: Only populated by init_ca, and only once: the operator's copy.
    root_key_pem: bytes | None = None

    @property
    def chain_pem(self) -> bytes:
        """Intermediate then root, the order a verifier expects."""
        return _pem(self.intermediate_cert) + _pem(self.root_cert)


def _pem(cert: x509.Certificate) -> bytes:
    return cert.public_bytes(serialization.Encoding.PEM)


def _name(common_name: str, org: str) -> x509.Name:
    return x509.Name(
        [
            x509.NameAttribute(NameOID.ORGANIZATION_NAME, org),
            x509.NameAttribute(NameOID.COMMON_NAME, common_name),
        ]
    )


def init_ca(directory: Path, *, passphrase: str, org: str = "Everwas") -> DeviceCa:
    """Create a root and an intermediate. Returns the root key ONCE.

    Idempotence is deliberately not offered: re-initialising would silently
    orphan every certificate already issued, and a fleet that cannot
    authenticate is not something to do by accident.
    """
    directory = Path(directory)
    if (directory / _INTERMEDIATE_KEY).exists():
        raise FileExistsError(
            f"{directory} already holds CA material; re-initialising would orphan "
            "every certificate already issued"
        )
    directory.mkdir(parents=True, exist_ok=True)

    now = dt.datetime.now(dt.UTC)
    root_key = ec.generate_private_key(ec.SECP256R1())
    root_subject = _name(f"{org} Device Root CA", org)
    root_cert = (
        x509.CertificateBuilder()
        .subject_name(root_subject)
        .issuer_name(root_subject)
        .public_key(root_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - dt.timedelta(minutes=5))
        .not_valid_after(now + CA_LIFETIME)
        # path_length=1: root signs an intermediate, the intermediate signs
        # leaves, and nothing signs below that.
        .add_extension(x509.BasicConstraints(ca=True, path_length=1), critical=True)
        .add_extension(_ca_key_usage(), critical=True)
        .add_extension(
            x509.SubjectKeyIdentifier.from_public_key(root_key.public_key()), critical=False
        )
        .sign(root_key, hashes.SHA256())
    )

    inter_key = ec.generate_private_key(ec.SECP256R1())
    inter_cert = (
        x509.CertificateBuilder()
        .subject_name(_name(f"{org} Device Issuing CA", org))
        .issuer_name(root_cert.subject)
        .public_key(inter_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - dt.timedelta(minutes=5))
        .not_valid_after(now + INTERMEDIATE_LIFETIME)
        .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
        .add_extension(_ca_key_usage(), critical=True)
        .add_extension(
            x509.SubjectKeyIdentifier.from_public_key(inter_key.public_key()), critical=False
        )
        .add_extension(
            x509.AuthorityKeyIdentifier.from_issuer_public_key(root_key.public_key()),
            critical=False,
        )
        .sign(root_key, hashes.SHA256())
    )

    (directory / _ROOT_CERT).write_bytes(_pem(root_cert))
    (directory / _INTERMEDIATE_CERT).write_bytes(_pem(inter_cert))
    (directory / _CHAIN).write_bytes(_pem(inter_cert) + _pem(root_cert))
    (directory / _INTERMEDIATE_KEY).write_bytes(
        inter_key.private_bytes(
            encoding=serialization.Encoding.PEM,
            format=serialization.PrivateFormat.PKCS8,
            encryption_algorithm=serialization.BestAvailableEncryption(passphrase.encode()),
        )
    )
    (directory / _INTERMEDIATE_KEY).chmod(0o600)

    # The root key is returned and NOT written. Anything that writes it here,
    # including a well-meant backup, undoes the reason for having a root at all.
    root_key_pem = root_key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.PKCS8,
        encryption_algorithm=serialization.BestAvailableEncryption(passphrase.encode()),
    )
    return DeviceCa(
        intermediate_cert=inter_cert,
        intermediate_key=inter_key,
        root_cert=root_cert,
        root_key_pem=root_key_pem,
    )


def _ca_key_usage() -> x509.KeyUsage:
    return x509.KeyUsage(
        digital_signature=True,
        content_commitment=False,
        key_encipherment=False,
        data_encipherment=False,
        key_agreement=False,
        key_cert_sign=True,
        crl_sign=True,
        encipher_only=False,
        decipher_only=False,
    )


def load_ca(directory: Path, *, passphrase: str) -> DeviceCa:
    """Load CA material and unlock the intermediate key."""
    directory = Path(directory)
    key_path = directory / _INTERMEDIATE_KEY
    if not key_path.exists():
        raise CaNotInitialisedError(f"no certificate authority in {directory}")

    key = serialization.load_pem_private_key(key_path.read_bytes(), password=passphrase.encode())
    return DeviceCa(
        intermediate_cert=x509.load_pem_x509_certificate(
            (directory / _INTERMEDIATE_CERT).read_bytes()
        ),
        intermediate_key=key,
        root_cert=x509.load_pem_x509_certificate((directory / _ROOT_CERT).read_bytes()),
    )


def build_csr(private_key, *, common_name: str) -> x509.CertificateSigningRequest:
    """Build a CSR. Lives here so tests and the agent agree on the shape."""
    return (
        x509.CertificateSigningRequestBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)]))
        .sign(private_key, hashes.SHA256())
    )


def issue_from_csr(
    ca: DeviceCa,
    csr: x509.CertificateSigningRequest,
    *,
    common_name: str,
    lifetime: dt.timedelta = CERT_LIFETIME,
) -> bytes:
    """Sign a CSR into a device certificate. Returns PEM.

    `common_name` is supplied by the CALLER, not read from the CSR. A CSR is
    attacker-controlled input: the device is asking for a key to be signed, and
    does not get to choose whose identity that key then carries. Reading the
    subject from the request would let an enrolled device request a certificate
    naming a different device.
    """
    if not csr.is_signature_valid:
        # Proof of possession. Without it, anyone can have a certificate issued
        # for a public key whose private half belongs to somebody else.
        raise CsrRejectedError("CSR signature is not valid")

    _reject_weak_key(csr.public_key())

    now = dt.datetime.now(dt.UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)]))
        .issuer_name(ca.intermediate_cert.subject)
        .public_key(csr.public_key())
        # Random, not sequential: a predictable serial leaks issuance volume and
        # makes certificates guessable by anything that indexes on them.
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - dt.timedelta(minutes=5))
        .not_valid_after(now + lifetime)
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
        .add_extension(
            x509.KeyUsage(
                digital_signature=True,
                content_commitment=False,
                key_encipherment=True,
                data_encipherment=False,
                key_agreement=False,
                key_cert_sign=False,
                crl_sign=False,
                encipher_only=False,
                decipher_only=False,
            ),
            critical=True,
        )
        # CLIENT_AUTH only. SERVER_AUTH here would let a device impersonate the
        # RADIUS server to any supplicant trusting this chain, which is how a
        # rogue access point harvests credentials.
        .add_extension(x509.ExtendedKeyUsage([ExtendedKeyUsageOID.CLIENT_AUTH]), critical=True)
        .add_extension(x509.SubjectKeyIdentifier.from_public_key(csr.public_key()), critical=False)
        .add_extension(
            x509.AuthorityKeyIdentifier.from_issuer_public_key(ca.intermediate_key.public_key()),
            critical=False,
        )
        .sign(ca.intermediate_key, hashes.SHA256())
    )
    return _pem(cert)


def _reject_weak_key(public_key) -> None:
    """Issuance is the last point at which a weak key can still be refused."""
    if isinstance(public_key, rsa.RSAPublicKey):
        if public_key.key_size < MIN_RSA_BITS:
            raise CsrRejectedError(
                f"RSA key is {public_key.key_size} bits; minimum is {MIN_RSA_BITS}"
            )
        return
    if isinstance(public_key, ec.EllipticCurvePublicKey):
        if public_key.curve.key_size < 256:
            raise CsrRejectedError(f"EC curve {public_key.curve.name} is too small")
        return
    raise CsrRejectedError(f"unsupported key type {type(public_key).__name__}")


def new_serial() -> int:
    """A serial for anything that needs one outside the builder."""
    return int.from_bytes(secrets.token_bytes(16), "big") >> 1


__all__ = [
    "CERT_LIFETIME",
    "RENEW_AFTER",
    "CaNotInitialisedError",
    "CsrRejectedError",
    "DeviceCa",
    "InvalidSignature",
    "build_csr",
    "init_ca",
    "issue_from_csr",
    "load_ca",
    "new_serial",
]
