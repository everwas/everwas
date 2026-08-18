"""The endpoint an agent calls to get its network certificate.

Same authentication as renewal: the credential the agent already holds. It
cannot be a session, because the thing asking is a machine, and it cannot be
the certificate itself, because this is how a machine that has none gets one.

The CSR is the only thing that crosses the wire. The private key is generated on
the endpoint and stays there, which is what makes it safe to do this over a
channel the server can already reach.
"""

import uuid

import pytest
from cryptography import x509
from cryptography.hazmat.primitives.asymmetric import ec

from openrmm.db.engine import session_scope
from openrmm.models.device import AgentCredential, Device, DeviceStatus, OsFamily
from openrmm.services.ca import build_csr, init_ca
from openrmm.services.enrollment import _sha256
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

SECRET = "agent-secret-for-cert-tests"


@pytest.fixture
def ca_dir(tmp_path, monkeypatch):
    from openrmm.config import get_settings

    get_settings.cache_clear()
    monkeypatch.setenv("OPENRMM_CA_DIR", str(tmp_path / "ca"))
    monkeypatch.setenv("OPENRMM_CA_PASSPHRASE", "test-ca-passphrase")
    init_ca(tmp_path / "ca", passphrase="test-ca-passphrase", org="Test Org")
    yield tmp_path / "ca"
    get_settings.cache_clear()


async def _enrolled(status: DeviceStatus = DeviceStatus.active) -> uuid.UUID:
    async with session_scope() as db:
        d = Device(
            id=uuid7(),
            hostname="csr-host",
            os_family=OsFamily.linux,
            tags=[],
            status=status,
        )
        db.add(d)
        await db.flush()
        db.add(AgentCredential(device_id=d.id, secret_hash=_sha256(SECRET)))
        return d.id


def _csr_pem(common_name: str = "anything") -> str:
    from cryptography.hazmat.primitives import serialization

    key = ec.generate_private_key(ec.SECP256R1())
    csr = build_csr(key, common_name=common_name)
    return csr.public_bytes(serialization.Encoding.PEM).decode()


async def test_an_enrolled_agent_receives_a_certificate_and_the_chain(client, ca_dir):
    device_id = await _enrolled()
    r = await client.post(
        "/api/v1/agents/certificate",
        json={"agent_id": str(device_id), "agent_secret": SECRET, "csr_pem": _csr_pem()},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert "BEGIN CERTIFICATE" in body["certificate_pem"]
    # The chain, or the device cannot present a path a verifier can build.
    assert body["chain_pem"].count("BEGIN CERTIFICATE") >= 2
    assert body["not_after"]

    cert = x509.load_pem_x509_certificate(body["certificate_pem"].encode())
    cn = cert.subject.get_attributes_for_oid(x509.oid.NameOID.COMMON_NAME)[0].value
    assert cn == str(device_id), "the identity must come from the credential, not the CSR"


async def test_a_wrong_secret_gets_no_certificate(client, ca_dir):
    device_id = await _enrolled()
    r = await client.post(
        "/api/v1/agents/certificate",
        json={
            "agent_id": str(device_id),
            "agent_secret": "wrong-but-long-enough-to-reach-the-check",
            "csr_pem": _csr_pem(),
        },
    )
    assert r.status_code == 403


async def test_a_retired_device_gets_no_certificate(client, ca_dir):
    """Retirement removes a machine from the fleet. A network identity would
    put it back through a different door."""
    device_id = await _enrolled(DeviceStatus.retired)
    r = await client.post(
        "/api/v1/agents/certificate",
        json={"agent_id": str(device_id), "agent_secret": SECRET, "csr_pem": _csr_pem()},
    )
    assert r.status_code in (403, 409)


async def test_a_malformed_csr_is_refused_cleanly(client, ca_dir):
    device_id = await _enrolled()
    r = await client.post(
        "/api/v1/agents/certificate",
        json={"agent_id": str(device_id), "agent_secret": SECRET, "csr_pem": "not a csr"},
    )
    assert r.status_code == 400, "a bad CSR is the caller's error, not a server crash"


async def test_the_response_never_contains_a_private_key(client, ca_dir):
    """The server has never held one and must never appear to.

    A response carrying key material would mean the endpoint generated it,
    which is the design this exists to avoid: a key that crossed the wire is a
    key that was in a log, a proxy buffer and a database somewhere.
    """
    device_id = await _enrolled()
    r = await client.post(
        "/api/v1/agents/certificate",
        json={"agent_id": str(device_id), "agent_secret": SECRET, "csr_pem": _csr_pem()},
    )
    assert "PRIVATE KEY" not in r.text
