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

from everwas.bitemporal.store import record_facts
from everwas.db.engine import get_sessionmaker, session_scope
from everwas.ingest.inventory import apply_inventory
from everwas.models.api_key import ApiKey
from everwas.models.device import Device, DeviceStatus, OsFamily, Site
from everwas.models.org import DEFAULT_ORG_ID, Organization
from everwas.security import sync_tokens
from everwas.security.api_keys import authenticate_key
from everwas.util.ids import uuid7

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
        principal = await authenticate_key(db, f"ewpk_{key_id}_{secret}")
    token, _ = sync_tokens.issue(principal)
    return {"Authorization": f"Bearer {token}"}


def client() -> httpx.AsyncClient:
    from everwas.api.app import create_app

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
    from everwas.security.api_keys import ApiKeyPrincipal

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


# --- posture ------------------------------------------------------------------------


POSTURE = {
    "checks": [
        {"check": "antivirus", "status": "not_applicable"},
        {"check": "disk-encryption", "status": "fail", "detail": "plaintext root"},
        {"check": "firewall", "status": "pass"},
    ]
}


async def test_posture_sweep_is_one_row_per_check_with_device_id():
    (device_id,) = await mk_devices(1)
    await ingest(device_id, "posture", POSTURE)
    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (await c.get("/api/v1/sync/posture", headers=headers)).json()
    rows = {i["check"]: i for i in body["items"]}
    assert set(rows) == {"antivirus", "disk-encryption", "firewall"}
    assert all(i["device_id"] == str(device_id) for i in body["items"])
    assert rows["disk-encryption"]["status"] == "fail"
    assert rows["disk-encryption"]["detail"] == "plaintext root"
    # not_applicable passes through untranslated: the check ran and could not
    # assess, which is not a failure and must not be collapsed into one.
    assert rows["antivirus"]["status"] == "not_applicable"
    # No detail given: "" (the verdict stands on its own), never null.
    assert rows["firewall"]["detail"] == ""


async def test_posture_sweep_pages_across_devices():
    ids = await mk_devices(3)
    for device_id in ids:
        await ingest(device_id, "posture", POSTURE)
    headers = await bearer(["devices:read"])
    seen, cursor = [], None
    async with client() as c:
        while True:
            params = {"limit": 2} | ({"cursor": cursor} if cursor else {})
            body = (await c.get("/api/v1/sync/posture", params=params, headers=headers)).json()
            seen.extend(body["items"])
            if not body["has_more"]:
                assert body["next_cursor"] is None
                break
            cursor = body["next_cursor"]
    # 3 devices x 3 checks, walked in (device_id, fact_key) order
    assert len(seen) == 9
    assert {i["device_id"] for i in seen} == {str(d) for d in ids}


async def test_posture_amend_and_as_of_time_travel():
    (device_id,) = await mk_devices(1)
    t0 = datetime.now(UTC) - timedelta(days=2)
    t1 = datetime.now(UTC) - timedelta(days=1)
    await ingest(device_id, "posture", {"checks": [{"check": "firewall", "status": "fail"}]}, at=t0)
    await ingest(device_id, "posture", {"checks": [{"check": "firewall", "status": "pass"}]}, at=t1)
    headers = await bearer(["devices:read"])
    async with client() as c:
        now = (await c.get("/api/v1/sync/posture", headers=headers)).json()
        assert [(i["check"], i["status"]) for i in now["items"]] == [("firewall", "pass")]

        then = (
            await c.get(
                "/api/v1/sync/posture",
                params={"as_of": (t0 + timedelta(hours=1)).isoformat()},
                headers=headers,
            )
        ).json()
        assert [(i["check"], i["status"]) for i in then["items"]] == [("firewall", "fail")]

        naive = await c.get(
            "/api/v1/sync/posture",
            params={"as_of": "2026-08-01T00:00:00"},
            headers=headers,
        )
        assert naive.status_code == 422
        assert "timezone" in naive.json()["detail"]


async def test_posture_sweep_is_org_scoped():
    (foreign,) = await mk_devices(1, org_id=OTHER_ORG)
    await ingest(foreign, "posture", POSTURE)
    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (await c.get("/api/v1/sync/posture", headers=headers)).json()
    assert body["items"] == []


async def test_posture_requires_a_token():
    async with client() as c:
        resp = await c.get("/api/v1/sync/posture")
    assert resp.status_code == 401


async def test_change_feed_covers_posture():
    # kind=posture rides the same FACT_TABLES dispatch as every other fact
    # kind — this proves it end to end: ingest, amend one check, and both
    # halves of the amend appear in the feed while the untouched check stays
    # out of it.
    (device_id,) = await mk_devices(1)
    await ingest(
        device_id,
        "posture",
        {
            "checks": [
                {"check": "firewall", "status": "fail", "detail": "nftables not loaded"},
                {"check": "disk-encryption", "status": "pass"},
            ]
        },
        at=datetime.now(UTC) - timedelta(days=2),
    )
    watermark = datetime.now(UTC)
    await ingest(
        device_id,
        "posture",
        {
            "checks": [
                {"check": "firewall", "status": "pass"},
                {"check": "disk-encryption", "status": "pass"},
            ]
        },
        at=datetime.now(UTC) - timedelta(days=1),
    )

    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (
            await c.get(
                "/api/v1/sync/changes",
                params={"kind": "posture", "since": watermark.isoformat()},
                headers=headers,
            )
        ).json()
    events = {(i["change"], i["fact_key"], i["payload"].get("status")) for i in body["items"]}
    # The old belief ended, the new one began; disk-encryption did not move.
    assert ("superseded", "check:firewall", "fail") in events
    assert ("recorded", "check:firewall", "pass") in events
    assert not any(key == "check:disk-encryption" for _, key, _ in events)


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

        from everwas.models.patch import ApprovalDecision, PatchCatalog
        from everwas.services.patching import approve

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


async def test_patch_identifier_prefers_kb_then_external_id():
    """Consumers key their patch catalogs on `identifier`; the precedence
    (first KB id, else external_id) is computed here so no consumer has to
    invent it."""
    (device_id,) = await mk_devices(1)
    await ingest(device_id, "patchstate", PATCHSTATE)
    headers = await bearer(["devices:read", "patches:read"])

    from sqlalchemy import select, update

    from everwas.models.patch import PatchCatalog

    async with client() as c:
        # No KB ids in the catalog yet -> identifier falls back to external_id.
        (item,) = (await c.get("/api/v1/sync/patches", headers=headers)).json()["items"]
        assert item["identifier"] == item["external_id"] == "KB5044284"

        async with session_scope() as db:
            catalog_id = (
                await db.execute(
                    select(PatchCatalog.id).where(PatchCatalog.external_id == "KB5044284")
                )
            ).scalar_one()
            await db.execute(
                update(PatchCatalog)
                .where(PatchCatalog.id == catalog_id)
                .values(kb_ids=["KB5044284", "KB5044285"])
            )
        (item,) = (await c.get("/api/v1/sync/patches", headers=headers)).json()["items"]
    assert item["identifier"] == "KB5044284"
    assert item["kb_ids"] == ["KB5044284", "KB5044285"]


# --- lifecycle and reachability ---------------------------------------------------


async def test_lifecycle_splits_durable_state_from_reachability():
    """status conflates 'retired' (a decision) with 'offline' (a 90-second
    heartbeat flap). lifecycle carries only the durable half; reachable
    carries only the volatile half, derived from the heartbeat timestamp."""
    fresh = datetime.now(UTC) - timedelta(seconds=5)
    stale = datetime.now(UTC) - timedelta(hours=3)

    async with get_sessionmaker()() as db, db.begin():
        db.add_all(
            [
                Device(
                    id=uuid7(),
                    hostname="never-seen",
                    os_family=OsFamily.linux,
                    status=DeviceStatus.enrolled,
                ),
                Device(
                    id=uuid7(),
                    hostname="humming",
                    os_family=OsFamily.linux,
                    status=DeviceStatus.active,
                    last_heartbeat_at=fresh,
                ),
                Device(
                    id=uuid7(),
                    hostname="quiet",
                    os_family=OsFamily.linux,
                    status=DeviceStatus.offline,
                    last_heartbeat_at=stale,
                ),
                Device(
                    id=uuid7(),
                    hostname="decommissioned",
                    os_family=OsFamily.linux,
                    status=DeviceStatus.retired,
                    last_heartbeat_at=stale,
                ),
            ]
        )

    headers = await bearer(["devices:read"])
    async with client() as c:
        items = (await c.get("/api/v1/sync/devices", headers=headers)).json()["items"]
    by_host = {i["hostname"]: i for i in items}

    assert by_host["never-seen"]["lifecycle"] == "enrolled"
    assert by_host["never-seen"]["reachable"] is None  # unknown, not unreachable

    assert by_host["humming"]["lifecycle"] == "operational"
    assert by_host["humming"]["reachable"] is True

    assert by_host["quiet"]["lifecycle"] == "operational"
    assert by_host["quiet"]["reachable"] is False

    assert by_host["decommissioned"]["lifecycle"] == "retired"
    assert by_host["decommissioned"]["reachable"] is False


# --- the change feed -------------------------------------------------------------


async def test_change_feed_reports_amend_as_superseded_plus_recorded():
    (device_id,) = await mk_devices(1)
    async with session_scope() as db:
        await record_facts(
            db,
            "software",
            device_id,
            {"pkg:openssl": {"version": "1.1"}},
            observed_at=datetime.now(UTC) - timedelta(days=2),
        )
    watermark = datetime.now(UTC)
    async with session_scope() as db:
        await record_facts(
            db,
            "software",
            device_id,
            {"pkg:openssl": {"version": "3.0"}},
            observed_at=datetime.now(UTC) - timedelta(days=1),
        )

    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (
            await c.get(
                "/api/v1/sync/changes",
                params={"kind": "software", "since": watermark.isoformat()},
                headers=headers,
            )
        ).json()
    events = [(i["change"], i["payload"].get("version")) for i in body["items"]]
    # The old belief ended, the new one began — both visible, in order.
    assert ("superseded", "1.1") in events
    assert ("recorded", "3.0") in events


async def test_change_feed_disappearance_is_superseded_only():
    # Partial removal: curl disappears while openssl stays. (A fully empty
    # snapshot is refused outright by the store's wholesale-retirement guard,
    # so a "remove everything" case cannot reach this feed.)
    (device_id,) = await mk_devices(1)
    async with session_scope() as db:
        await record_facts(
            db,
            "software",
            device_id,
            {"pkg:curl": {"version": "8.0"}, "pkg:openssl": {"version": "3.0"}},
            observed_at=datetime.now(UTC) - timedelta(days=2),
        )
    watermark = datetime.now(UTC)
    async with session_scope() as db:
        await record_facts(
            db,
            "software",
            device_id,
            {"pkg:openssl": {"version": "3.0"}},
            observed_at=datetime.now(UTC),
        )

    headers = await bearer(["devices:read"])
    async with client() as c:
        body = (
            await c.get(
                "/api/v1/sync/changes",
                params={"kind": "software", "since": watermark.isoformat()},
                headers=headers,
            )
        ).json()
    # A removal is two events: the open-ended belief is superseded, and a
    # tombstone is recorded whose valid_to is set — the server's new belief
    # that curl WAS there until now. A consumer keeps a fact as current only
    # while its latest recorded event has valid_to == null.
    events = {(i["change"], i["fact_key"]): i for i in body["items"]}
    assert set(events) == {("superseded", "pkg:curl"), ("recorded", "pkg:curl")}
    assert events[("superseded", "pkg:curl")]["valid_to"] is None
    assert events[("recorded", "pkg:curl")]["valid_to"] is not None


async def test_change_feed_refuses_naive_since_and_pages():
    ids = await mk_devices(2)
    watermark = datetime.now(UTC)
    for device_id in ids:
        await ingest(device_id, "network", INTERFACES)

    headers = await bearer(["devices:read"])
    async with client() as c:
        naive = await c.get(
            "/api/v1/sync/changes",
            params={"kind": "network", "since": "2026-08-01T00:00:00"},
            headers=headers,
        )
        assert naive.status_code == 422

        seen, cursor = [], None
        while True:
            params = {"kind": "network", "since": watermark.isoformat(), "limit": 3} | (
                {"cursor": cursor} if cursor else {}
            )
            body = (await c.get("/api/v1/sync/changes", params=params, headers=headers)).json()
            seen.extend(body["items"])
            if not body["has_more"]:
                assert body["next_cursor"] is None
                break
            cursor = body["next_cursor"]
    # 2 devices x 2 interfaces, all freshly recorded
    assert len(seen) == 4
    assert {i["change"] for i in seen} == {"recorded"}
