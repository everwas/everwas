"""Devices holding a different certificate from the one we last issued them.

Knowing what we issued and knowing what the machine has are different facts.
They diverge on a renewal that was issued and never saved, on a machine
restored from a backup image or cloned from a template, and on material deleted
by hand. Until the agent reports what it holds, all three are invisible until
they surface as an authentication failure nobody can account for.
"""

import datetime as dt
import uuid

import pytest

from everwas.db.engine import session_scope
from everwas.ingest.heartbeat import apply_heartbeat
from everwas.models.certificate import DeviceCertificate
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.services.certificates import DriftKind, certificate_drift
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _device(hostname: str = "drift-host") -> uuid.UUID:
    async with session_scope() as db:
        d = Device(
            id=uuid7(),
            hostname=hostname,
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
        )
        db.add(d)
        await db.flush()
        return d.id


async def _issue(device_id: uuid.UUID, serial: str, *, days_ago: int = 0) -> None:
    """Record a certificate as issued, without doing the crypto."""
    now = dt.datetime.now(dt.UTC)
    async with session_scope() as db:
        db.add(
            DeviceCertificate(
                serial=serial,
                device_id=device_id,
                common_name=str(device_id),
                certificate_pem="-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n",
                fingerprint_sha256=serial * 4,
                not_before=now - dt.timedelta(days=days_ago),
                not_after=now + dt.timedelta(days=90 - days_ago),
            )
        )


async def _reports(device_id: uuid.UUID, serial: str | None) -> None:
    """Deliver a heartbeat saying what the device holds."""
    data: dict = {"version": "test"}
    if serial is not None:
        data["cert_serial"] = serial
        data["cert_not_after"] = (dt.datetime.now(dt.UTC) + dt.timedelta(days=45)).isoformat()
    else:
        data["cert_serial"] = ""
    async with session_scope() as db:
        await apply_heartbeat(db, device_id, data)


async def _drift() -> list:
    async with session_scope() as db:
        return await certificate_drift(db)


async def test_a_device_holding_what_we_issued_is_not_drift():
    device_id = await _device()
    await _issue(device_id, "aaaa")
    await _reports(device_id, "aaaa")

    assert await _drift() == []


async def test_a_device_still_on_the_previous_certificate_is_stale():
    # The renewal was issued and the device never saved it: a lost response, a
    # full disk, a process killed between the two. The server believes this
    # machine is fine and it is running on a certificate that expires first.
    device_id = await _device()
    await _issue(device_id, "old", days_ago=60)
    await _issue(device_id, "new")
    await _reports(device_id, "old")

    drift = await _drift()
    assert len(drift) == 1
    assert drift[0].kind is DriftKind.stale
    assert drift[0].reported_serial == "old"
    assert drift[0].issued_serial == "new"


async def test_a_device_holding_something_we_never_issued_is_unknown():
    # A machine cloned from an image that carried somebody else's certificate,
    # or one enrolled against a different server. It authenticates perfectly
    # well, which is exactly why nobody notices.
    device_id = await _device()
    await _issue(device_id, "ours")
    await _reports(device_id, "somebody-elses")

    drift = await _drift()
    assert len(drift) == 1
    assert drift[0].kind is DriftKind.unknown


async def test_a_device_reporting_nothing_while_issued_something_is_missing():
    device_id = await _device()
    await _issue(device_id, "aaaa")
    await _reports(device_id, None)

    drift = await _drift()
    assert len(drift) == 1
    assert drift[0].kind is DriftKind.missing


async def test_an_agent_that_never_reports_is_not_treated_as_drift():
    # An agent too old to report is not evidence that anything is wrong. A
    # fleet mid-upgrade would otherwise light up entirely with a finding that
    # means "your agents are old", which is true, unrelated, and would bury the
    # real ones.
    device_id = await _device()
    await _issue(device_id, "aaaa")
    async with session_scope() as db:
        await apply_heartbeat(db, device_id, {"version": "old-agent"})

    assert await _drift() == []


async def test_an_absent_field_does_not_erase_what_we_already_knew():
    # Half a fleet reporting and half not is the ordinary state during a
    # rollout. If an old agent's heartbeat cleared the column, the reported
    # serial would be wiped every thirty seconds by whichever agent beat last,
    # destroying the signal this exists to carry.
    device_id = await _device()
    await _issue(device_id, "aaaa")
    await _reports(device_id, "aaaa")

    async with session_scope() as db:
        await apply_heartbeat(db, device_id, {"version": "old-agent"})

    async with session_scope() as db:
        device = await db.get(Device, device_id)
        assert device.reported_cert_serial == "aaaa", (
            "an agent too old to report cleared what a newer one had told us"
        )


async def test_a_revoked_certificate_does_not_count_as_currently_issued():
    # current_certificate excludes revoked ones, so a device still holding a
    # revoked certificate must show as drift rather than as a match.
    device_id = await _device()
    await _issue(device_id, "revoked-one")
    async with session_scope() as db:
        cert = await db.get(DeviceCertificate, "revoked-one")
        cert.revoked_at = dt.datetime.now(dt.UTC)
        cert.revocation_reason = "keyCompromise"
    await _reports(device_id, "revoked-one")

    drift = await _drift()
    assert len(drift) == 1
    assert drift[0].kind is DriftKind.stale


async def test_the_drift_route_is_reachable(client):
    # FastAPI matches routes in registration order, so a literal path declared
    # after /{device_id} is shadowed by it: "certificate-drift" gets parsed as
    # a device id and rejected as a malformed UUID. The endpoint would 422
    # forever while looking entirely correct in the source, which is why this
    # asserts on reachability rather than on any particular body.
    r = await client.get("/api/v1/devices/certificate-drift")
    assert r.status_code == 200, r.text
    assert isinstance(r.json(), list)
