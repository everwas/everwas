"""POST /patches/deploy regression.

The deploy route called `_device_or_404(db, body.device_id)` without the
`user` argument, so every deploy raised TypeError -> 500 before the org scope
or approval checks ever ran. These tests pin the two paths through the fixed
call: a known device proceeds to the approval check (400, nothing approved),
an unknown device 404s.
"""

import uuid

import pytest

from everwas.db.engine import session_scope
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def _device() -> uuid.UUID:
    async with session_scope() as db:
        d = Device(
            id=uuid7(),
            hostname="deploy-target",
            os_family=OsFamily.linux,
            tags=[],
            status=DeviceStatus.active,
        )
        db.add(d)
        await db.flush()
        return d.id


async def test_deploy_without_approvals_is_400_not_500(client):
    device_id = await _device()
    resp = await client.post(
        "/api/v1/patches/deploy", json={"device_id": str(device_id), "external_ids": []}
    )
    assert resp.status_code == 400
    assert "approved" in resp.json()["detail"]


async def test_deploy_unknown_device_is_404(client):
    resp = await client.post(
        "/api/v1/patches/deploy", json={"device_id": str(uuid7()), "external_ids": []}
    )
    assert resp.status_code == 404
