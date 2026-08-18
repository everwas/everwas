"""/api/v1/sync behaves like its contract: same envelope everywhere, keyset
cursors that terminate unambiguously, org isolation, and bitemporal params
that refuse naive datetimes.

The consumer these tests stand in for is a reconciling SSoT job: for it,
a null where [] belongs reads as "everything was deleted", and a silently
mis-walked cursor reads as truth. Hence the pedantry here.
"""

import hashlib
import secrets
import uuid
from datetime import UTC, datetime, timedelta

import httpx
import pytest

from openrmm.bitemporal.store import record_facts
from openrmm.db.engine import get_sessionmaker, session_scope
from openrmm.ingest.inventory import apply_inventory
from openrmm.models.api_key import ApiKey
from openrmm.models.device import Device, DeviceStatus, OsFamily, Site
from openrmm.models.org import DEFAULT_ORG_ID, Organization
from openrmm.security import sync_tokens
from openrmm.security.api_keys import authenticate_key
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

OTHER_ORG = uuid.UUID("0000000b-0000-0000-0000-00000000000b")


async def bearer(scopes: list[str], org_id: uuid.UUID | None = DEFAULT_ORG_ID) -> dict:
    key_id = secrets.token_hex(11)
    secret = secrets.token_urlsafe(32)
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            ApiKey(
                name="sync-test",
                key_id=key_id,
                secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
                scopes=scopes,
                org_id=org_id,
            )
        )
    async with session_scope() as db:
        principal = await authenticate_key(db, f"orpk_{key_id}_{secret}")
    token, _ = sync_tokens.issue(principal)
    return {"Authorization": f"Bearer {token}"}


def client() -> httpx.AsyncClient:
    from openrmm.api.app import create_app

    return httpx.AsyncClient(
        transport=httpx.ASGITransport(app=create_app()), base_url="http://test"
    )


async def mk_devices(n: int, org_id: uuid.UUID = DEFAULT_ORG_ID, **kwargs) -> list[uuid.UUID]:
    ids = []
    async with get_sessionmaker()() as db, db.begin():
        if org_id != DEFAULT_ORG_ID and await db.get(Organization, org_id) is None:
            db.add(Organization(id=org_id, name=f"org-{org_id.hex[-4:]}"))
            await db.flush()
        for i in range(n):
            d = Device(
                id=uuid7(),
                hostname=f"host-{i:03d}",
                os_family=OsFamily.linux,
                tags=[],
                status=DeviceStatus.active,
                org_id=org_id,
                **kwargs,
            )
            db.add(d)
            ids.append(d.id)
    return ids


HW = {
    "cpu_model": "AMD Ryzen 7",
    "cpu_cores": 16,
    "mem_total": 68719476736,
    "hostname": "host-000",
    "os_family": "linux",
    "os_version": "Arch Linux",
    "kernel": "6.9.1",
    "arch": "x86_64",
    "virtualization": "",
}

DMI = {
    "manufacturer": "LENOVO",
    "model": "21CB000JUS",
    "serial_number": "PF3K2ABC",
    "chassis_type": "laptop",
}

INTERFACES = {
    "interfaces": [
        {
            "name": "lo",
            "mac": "",
            "mtu": 65536,
            "up": True,
            "loopback": True,
            "addresses": ["127.0.0.1/8"],
        },
        {
            "name": "eth0",
            "mac": "aa:bb:cc:dd:ee:01",
            "mtu": 1500,
            "up": True,
            "loopback": False,
            "addresses": ["192.0.2.10/24"],
        },
    ]
}


async def ingest(device_id: uuid.UUID, kind: str, data: dict, at: datetime | None = None):
    async with session_scope() as db:
        await apply_inventory(db, device_id, kind, at or datetime.now(UTC), dict(data))


# --- pagination conformance --------------------------------------------------


async def test_device_walk_terminates_and_cursors_advance():
    await mk_devices(25)
    headers = await bearer(["devices:read"])
    pages, cursors, cursor = [], [], None
    async with client() as c:
        while True:
            params = {"limit": 10} | ({"cursor": cursor} if cursor else {})
            resp = await c.get("/api/v1/sync/devices", params=params, headers=headers)
            assert resp.status_code == 200
            body = resp.json()
            pages.append(len(body["items"]))
            if not body["has_more"]:
                assert body["next_cursor"] is None
                break
            assert body["next_cursor"] is not None
            assert body["next_cursor"] not in cursors, "cursor did not advance"
            cursors.append(cursor := body["next_cursor"])
    assert pages == [10, 10, 5]


async def test_empty_page_is_a_literal_empty_array():
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get("/api/v1/sync/devices", headers=headers)
    # A reconciling consumer reads null as "everything is gone"; assert on
    # the raw JSON text so a serializer regression cannot hide behind ==.
    assert resp.json()["items"] == []
    assert '"items":[]' in resp.text.replace(" ", "")


async def test_garbage_cursor_is_422():
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get(
            "/api/v1/sync/devices", params={"cursor": "not-base64!!"}, headers=headers
        )
        assert resp.status_code == 422
        # A device cursor (1 part) is not an interface cursor (2 parts).
        dev = await c.get("/api/v1/sync/devices", params={"limit": 1}, headers=headers)
        assert dev.status_code == 200


async def test_cursor_is_not_portable_between_endpoints():
    await mk_devices(3)
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get("/api/v1/sync/devices", params={"limit": 1}, headers=headers)
        device_cursor = resp.json()["next_cursor"]
        assert device_cursor
        wrong = await c.get(
            "/api/v1/sync/interfaces", params={"cursor": device_cursor}, headers=headers
        )
    assert wrong.status_code == 422


# --- the device payload --------------------------------------------------------


async def test_device_payload_with_identity_and_addresses():
    (device_id,) = await mk_devices(1)
    await ingest(device_id, "hardware", HW | DMI)
    await ingest(device_id, "network", INTERFACES)
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get("/api/v1/sync/devices", headers=headers)
    (item,) = resp.json()["items"]
    assert item["serial_number"] == "PF3K2ABC"
    assert item["manufacturer"] == "LENOVO"
    assert item["model"] == "21CB000JUS"
    assert item["chassis_type"] == "laptop"
    assert item["device_class"] == "laptop"
    assert item["is_virtual"] is False
    assert item["cpu_model"] == "AMD Ryzen 7"
    assert item["memory_bytes"] == 68719476736
    # loopback excluded, CIDR preserved
    assert item["mac_addresses"] == ["aa:bb:cc:dd:ee:01"]
    assert item["ip_addresses"] == ["192.0.2.10/24"]


async def test_device_without_facts_reports_unknown_not_empty():
    await mk_devices(1)
    headers = await bearer(["devices:read"])
    async with client() as c:
        (item,) = (await c.get("/api/v1/sync/devices", headers=headers)).json()["items"]
    assert item["serial_number"] is None
    assert item["manufacturer"] is None
    assert item["is_virtual"] is None
    assert item["device_class"] is None
    assert item["mac_addresses"] == []


async def test_virtual_machine_classifies_as_vm():
    (device_id,) = await mk_devices(1)
    await ingest(device_id, "hardware", HW | {"virtualization": "kvm"})
    headers = await bearer(["devices:read"])
    async with client() as c:
        (item,) = (await c.get("/api/v1/sync/devices", headers=headers)).json()["items"]
    assert item["is_virtual"] is True
    assert item["device_class"] == "vm"


async def test_site_filter():
    async with get_sessionmaker()() as db, db.begin():
        site = Site(name="hq", org_id=DEFAULT_ORG_ID)
        db.add(site)
        await db.flush()
        site_id = site.id
    await mk_devices(2)
    await mk_devices(1, site_id=site_id)
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get(
            "/api/v1/sync/devices", params={"site_id": str(site_id)}, headers=headers
        )
    assert len(resp.json()["items"]) == 1
    assert resp.json()["items"][0]["site_id"] == str(site_id)


# --- org isolation ---------------------------------------------------------------


async def test_foreign_org_is_invisible():
    await mk_devices(2)
    await mk_devices(3, org_id=OTHER_ORG)
    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (await c.get("/api/v1/sync/devices", headers=headers)).json()
    assert len(body["items"]) == 2


async def test_orgless_principal_fails_closed_to_empty_pages():
    """org_id is NOT NULL since migration 0016, so a real key always has an
    org — but the token layer could still produce an org-less principal if
    the claims path grew a bug. scope_to_org fails closed for exactly that
    case: empty pages, never the whole fleet."""
    await mk_devices(2)
    from openrmm.security.api_keys import ApiKeyPrincipal

    # A real key row so the revocation lookup passes, an org-less principal
    # in the claims.
    key_id = secrets.token_hex(11)
    async with get_sessionmaker()() as db, db.begin():
        key = ApiKey(
            name="orgless",
            key_id=key_id,
            secret_hash=hashlib.sha256(b"x").hexdigest(),
            scopes=["devices:read"],
        )
        db.add(key)
        await db.flush()
        row_id = key.id
    token, _ = sync_tokens.issue(
        ApiKeyPrincipal(id=str(row_id), name="orgless", scopes=("devices:read",), org_id=None)
    )
    async with client() as c:
        body = (
            await c.get("/api/v1/sync/devices", headers={"Authorization": f"Bearer {token}"})
        ).json()
    assert body == {"items": [], "has_more": False, "next_cursor": None}


async def test_fact_sweeps_are_org_scoped():
    (foreign,) = await mk_devices(1, org_id=OTHER_ORG)
    await ingest(foreign, "network", INTERFACES)
    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (await c.get("/api/v1/sync/interfaces", headers=headers)).json()
    assert body["items"] == []


# --- fleet-wide fact sweeps -------------------------------------------------------


async def test_interface_sweep_carries_device_id_and_pages_across_devices():
    ids = await mk_devices(3)
    for device_id in ids:
        await ingest(device_id, "network", INTERFACES)
    headers = await bearer(["devices:read"])
    seen, cursor = [], None
    async with client() as c:
        while True:
            params = {"limit": 2} | ({"cursor": cursor} if cursor else {})
            body = (await c.get("/api/v1/sync/interfaces", params=params, headers=headers)).json()
            seen.extend(body["items"])
            if not body["has_more"]:
                break
            cursor = body["next_cursor"]
    # 3 devices x 2 interfaces, walked in (device_id, fact_key) order
    assert len(seen) == 6
    assert {i["device_id"] for i in seen} == {str(d) for d in ids}
    eth0 = [i for i in seen if i["key"] == "eth0"]
    assert all(i["mac"] == "aa:bb:cc:dd:ee:01" for i in eth0)
    assert all(i["addresses"] == ["192.0.2.10/24"] for i in eth0)


async def test_software_sweep_and_as_of_time_travel():
    (device_id,) = await mk_devices(1)
    t0 = datetime.now(UTC) - timedelta(days=2)
    t1 = datetime.now(UTC) - timedelta(days=1)
    async with session_scope() as db:
        await record_facts(
            db, "software", device_id, {"pkg:openssl": {"version": "1.1"}}, observed_at=t0
        )
    async with session_scope() as db:
        await record_facts(
            db, "software", device_id, {"pkg:openssl": {"version": "3.0"}}, observed_at=t1
        )
    headers = await bearer(["devices:read"])
    async with client() as c:
        now = (await c.get("/api/v1/sync/software", headers=headers)).json()
        assert [(i["name"], i["version"]) for i in now["items"]] == [("openssl", "3.0")]

        then = (
            await c.get(
                "/api/v1/sync/software",
                params={"as_of": (t0 + timedelta(hours=1)).isoformat()},
                headers=headers,
            )
        ).json()
        assert [(i["name"], i["version"]) for i in then["items"]] == [("openssl", "1.1")]

        naive = await c.get(
            "/api/v1/sync/software",
            params={"as_of": "2026-08-01T00:00:00"},
            headers=headers,
        )
        assert naive.status_code == 422
        assert "timezone" in naive.json()["detail"]


# --- patches ------------------------------------------------------------------------


PATCHSTATE = {
    "patches": [
        {
            "id": "KB5044284",
            "title": "Cumulative Update",
            "kind": "security",
            "severity": "critical",
            "reboot_likely": True,
        }
    ]
}


async def test_patch_sweep_catalog_join_and_approval_status():
    (device_id,) = await mk_devices(1)
    await ingest(device_id, "patchstate", PATCHSTATE)
    headers = await bearer(["devices:read", "patches:read"])
    async with client() as c:
        body = (await c.get("/api/v1/sync/patches", headers=headers)).json()
        (item,) = body["items"]
        assert item["device_id"] == str(device_id)
        assert item["external_id"] == "KB5044284"
        assert item["title"] == "Cumulative Update"
        assert item["status"] == "pending"

        # Approve it fleet-wide through the service and watch status flip.
        from sqlalchemy import select

        from openrmm.models.patch import ApprovalDecision, PatchCatalog
        from openrmm.services.patching import approve

        async with session_scope() as db:
            catalog_id = (
                await db.execute(
                    select(PatchCatalog.id).where(PatchCatalog.external_id == "KB5044284")
                )
            ).scalar_one()
            await approve(
                db,
                catalog_id,
                device_id=None,
                decision=ApprovalDecision.approved,
                decided_by="test",
                org_id=DEFAULT_ORG_ID,
            )
        body = (await c.get("/api/v1/sync/patches", headers=headers)).json()
    assert body["items"][0]["status"] == "approved"


async def test_patches_requires_its_own_scope():
    headers = await bearer(["devices:read"])
    async with client() as c:
        resp = await c.get("/api/v1/sync/patches", headers=headers)
    assert resp.status_code == 403
