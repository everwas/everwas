"""MCP server tests: authentication, scopes, filtering, the two time axes, and
the promise that a dry run queues nothing.

Everything runs through the in-memory FastMCP client, so there is no network
and no live server, but the real tool bodies and the real database are in play.
"""

import asyncio
import hashlib
import secrets
import uuid
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta

import pytest
from fastmcp import Client
from fastmcp.exceptions import ToolError
from mcp.server.auth.middleware.auth_context import auth_context_var
from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
from sqlalchemy import func, select

from everwas.bitemporal.store import record_facts
from everwas.db.engine import get_sessionmaker
from everwas.mcp.context import authenticate, hash_secret, iso, parse_api_key
from everwas.mcp.server import ApiKeyVerifier, mcp
from everwas.models.alert import Alert, AlertRule, AlertState, Metric, Severity
from everwas.models.api_key import ApiKey
from everwas.models.audit import AuditLog
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.models.patch import PatchApproval, PatchCatalog, PatchSeverity
from everwas.models.script import RunStatus, Script, ScriptRun, ShellKind
from everwas.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")

READ_ONLY = ["devices:read", "alerts:read", "patches:read"]
FULL = [*READ_ONLY, "alerts:write", "scripts:run", "patches:write"]


# --- fixtures and helpers -------------------------------------------------


async def mint_key(name: str, scopes: list[str], *, expires_in: timedelta | None = None) -> str:
    """Create an API key row the way `everwas create-api-key` does."""
    key_id = secrets.token_hex(11)
    secret = secrets.token_urlsafe(32)
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            ApiKey(
                name=name,
                key_id=key_id,
                secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
                scopes=scopes,
                expires_at=(datetime.now(UTC) + expires_in if expires_in else None),
            )
        )
    return f"ewpk_{key_id}_{secret}"


@asynccontextmanager
async def client_for(raw_key: str):
    """Verify a key the way the HTTP transport does, then open an in-memory client.

    The access token has to be in context BEFORE the client starts: the
    in-memory server runs in a task that inherits the context at creation time,
    so anything set afterwards is invisible to the tools.
    """
    token = await ApiKeyVerifier().verify_token(raw_key)
    assert token is not None, "key failed verification"
    reset = auth_context_var.set(AuthenticatedUser(token))
    try:
        async with Client(mcp) as client:
            yield client
    finally:
        auth_context_var.reset(reset)


async def add_device(hostname: str, **kwargs) -> Device:
    device = Device(
        id=uuid7(),
        hostname=hostname,
        os_family=kwargs.pop("os_family", OsFamily.linux),
        status=kwargs.pop("status", DeviceStatus.active),
        tags=kwargs.pop("tags", []),
        **kwargs,
    )
    async with get_sessionmaker()() as db, db.begin():
        db.add(device)
    return device


async def audit_rows(action: str) -> list[AuditLog]:
    async with get_sessionmaker()() as db:
        stmt = select(AuditLog).where(AuditLog.action == action).order_by(AuditLog.at)
        return list((await db.execute(stmt)).scalars())


async def count(model, *where) -> int:
    async with get_sessionmaker()() as db:
        stmt = select(func.count()).select_from(model).where(*where)
        return (await db.execute(stmt)).scalar_one()


# --- authentication -------------------------------------------------------


def test_parse_api_key_shapes():
    assert parse_api_key("ewpk_abc_def") == ("abc", "def")
    assert parse_api_key("ewpk_abc_def_ghi") == ("abc", "def_ghi")
    assert parse_api_key("bearer-token") is None
    assert parse_api_key("ewpk_only") is None
    assert parse_api_key("") is None


async def test_authenticate_unknown_key():
    assert await authenticate("ewpk_deadbeefdeadbeefdeadbe_nope") is None
    assert await authenticate("not-an-everwas-key") is None


async def test_authenticate_wrong_secret():
    raw = await mint_key("wrong-secret", READ_ONLY)
    key_id, _ = parse_api_key(raw)
    assert await authenticate(f"ewpk_{key_id}_{secrets.token_urlsafe(32)}") is None


async def test_authenticate_expired_key():
    raw = await mint_key("expired", READ_ONLY, expires_in=timedelta(days=-1))
    assert await authenticate(raw) is None


async def test_authenticate_valid_key_records_use():
    raw = await mint_key("valid", READ_ONLY)
    principal = await authenticate(raw)
    assert principal is not None
    assert principal.name == "valid"
    assert principal.scopes == tuple(READ_ONLY)

    key_id, secret = parse_api_key(raw)
    async with get_sessionmaker()() as db:
        row = (await db.execute(select(ApiKey).where(ApiKey.key_id == key_id))).scalar_one()
    assert row.last_used_at is not None
    assert row.secret_hash == hash_secret(secret)
    # the raw secret is never echoed back on the principal
    assert secret not in repr(principal)


async def test_unauthenticated_call_is_refused_and_audited():
    await add_device("anon-probe")
    async with Client(mcp) as client:  # no access token in context
        with pytest.raises(ToolError) as exc:
            await client.call_tool("list_devices", {})
    assert "Unauthenticated" in str(exc.value)

    rows = await audit_rows("mcp.list_devices")
    assert [r.detail["ok"] for r in rows] == [False]
    assert rows[0].actor_id is None


# --- scopes ---------------------------------------------------------------


async def test_read_only_key_cannot_run_a_script():
    device = await add_device("script-target", tags=["prod"])
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            Script(
                name="reboot-check",
                shell=ShellKind.bash,
                body="uptime",
                sha256="0" * 64,
            )
        )

    raw = await mint_key("read-only", READ_ONLY)
    async with client_for(raw) as client:
        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "run_script",
                {"script_name": "reboot-check", "device_ids": [str(device.id)], "confirm": True},
            )
    message = str(exc.value)
    assert "scripts:run" in message
    assert "devices:read" in message  # tells the assistant what the key does have

    assert await count(ScriptRun) == 0
    rows = await audit_rows("mcp.run_script")
    assert [r.detail["ok"] for r in rows] == [False]
    assert rows[0].actor_id == "read-only"


# --- fleet reads ----------------------------------------------------------


async def test_list_devices_filtering():
    await add_device("web-01", tags=["prod", "linux"])
    await add_device("web-02", status=DeviceStatus.offline, tags=["prod"])
    await add_device("laptop-jane", os_family=OsFamily.macos, tags=["laptops"])

    raw = await mint_key("reader", READ_ONLY)
    async with client_for(raw) as client:
        everything = (await client.call_tool("list_devices", {})).data
        assert {d["hostname"] for d in everything} == {"web-01", "web-02", "laptop-jane"}

        offline = (await client.call_tool("list_devices", {"status": "offline"})).data
        assert [d["hostname"] for d in offline] == ["web-02"]

        prod = (await client.call_tool("list_devices", {"tag": "prod"})).data
        assert {d["hostname"] for d in prod} == {"web-01", "web-02"}

        webs = (await client.call_tool("list_devices", {"hostname_contains": "WEB"})).data
        assert {d["hostname"] for d in webs} == {"web-01", "web-02"}

        with pytest.raises(ToolError) as exc:
            await client.call_tool("list_devices", {"status": "banana"})
        assert "enrolled" in str(exc.value)  # the error names the valid values

    # a successful read tool audits too
    rows = await audit_rows("mcp.list_devices")
    assert len(rows) == 5
    assert [r.detail["ok"] for r in rows] == [True, True, True, True, False]
    assert rows[0].actor_id == "reader"
    assert rows[0].detail["count"] == 3


async def test_get_device_returns_detail():
    device = await add_device("detail-01", agent_version="1.2.3", arch="amd64")
    raw = await mint_key("reader", READ_ONLY)
    async with client_for(raw) as client:
        out = (await client.call_tool("get_device", {"device_id": str(device.id)})).data
        assert out["hostname"] == "detail-01"
        assert out["agent_version"] == "1.2.3"
        assert out["arch"] == "amd64"
        assert out["enrolled_at"].endswith("Z")

        with pytest.raises(ToolError) as exc:
            await client.call_tool("get_device", {"device_id": str(uuid.uuid4())})
        assert "No device" in str(exc.value)

        with pytest.raises(ToolError) as exc:
            await client.call_tool("get_device", {"device_id": "web-01"})
        assert "must be a UUID" in str(exc.value)


# --- the two time axes ----------------------------------------------------


async def test_get_device_facts_valid_time_and_record_time_differ():
    device = await add_device("timemachine-01")
    t0 = datetime.now(UTC) - timedelta(days=3)
    t1 = datetime.now(UTC) - timedelta(days=2)
    sm = get_sessionmaker()

    async with sm() as db, db.begin():
        await record_facts(
            db,
            "software",
            device.id,
            {"pkg:openssl": {"version": "1.1"}, "pkg:curl": {"version": "8.0"}},
            observed_at=t0,
        )
    knew_after_t0 = datetime.now(UTC)
    await asyncio.sleep(0.05)

    async with sm() as db, db.begin():
        await record_facts(
            db,
            "software",
            device.id,
            {"pkg:openssl": {"version": "3.0"}, "pkg:curl": {"version": "8.0"}},
            observed_at=t1,
        )

    raw = await mint_key("historian", READ_ONLY)
    async with client_for(raw) as client:
        now_facts = (await client.call_tool("get_device_facts", {"device_id": str(device.id)})).data
        versions = {f["fact_key"]: f["payload"]["version"] for f in now_facts["facts"]}
        assert versions["pkg:openssl"] == "3.0"
        assert now_facts["count"] == 2
        assert now_facts["truncated"] is False

        after_upgrade = iso(t1 + timedelta(hours=1))

        # valid time only: today's knowledge says openssl was 3.0 by then
        valid_only = (
            await client.call_tool(
                "get_device_facts", {"device_id": str(device.id), "as_of": after_upgrade}
            )
        ).data
        assert {f["fact_key"]: f["payload"]["version"] for f in valid_only["facts"]}[
            "pkg:openssl"
        ] == "3.0"

        # same instant, but only what we believed right after the first report
        believed = (
            await client.call_tool(
                "get_device_facts",
                {
                    "device_id": str(device.id),
                    "as_of": after_upgrade,
                    "knew_at": iso(knew_after_t0),
                },
            )
        ).data
        assert {f["fact_key"]: f["payload"]["version"] for f in believed["facts"]}[
            "pkg:openssl"
        ] == "1.1"
        assert believed["as_of"] == after_upgrade
        assert believed["knew_at"] == iso(knew_after_t0)


async def test_get_device_facts_rejects_naive_timestamps():
    device = await add_device("timemachine-02")
    raw = await mint_key("historian", READ_ONLY)
    async with client_for(raw) as client:
        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "get_device_facts",
                {"device_id": str(device.id), "as_of": "2026-08-01T13:00:00"},
            )
        assert "no timezone" in str(exc.value)

        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "get_device_facts",
                {"device_id": str(device.id), "knew_at": "last tuesday"},
            )
        assert "ISO-8601" in str(exc.value)

        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "get_device_facts", {"device_id": str(device.id), "kind": "everything"}
            )
        assert "patchstate" in str(exc.value)


async def test_diff_device_facts_reports_change():
    device = await add_device("diff-01")
    t0 = datetime.now(UTC) - timedelta(days=3)
    t1 = datetime.now(UTC) - timedelta(days=2)
    sm = get_sessionmaker()

    async with sm() as db, db.begin():
        await record_facts(
            db, "software", device.id, {"pkg:openssl": {"version": "1.1"}}, observed_at=t0
        )
    async with sm() as db, db.begin():
        await record_facts(
            db,
            "software",
            device.id,
            {"pkg:openssl": {"version": "3.0"}, "pkg:jq": {"version": "1.7"}},
            observed_at=t1,
        )

    raw = await mint_key("historian", READ_ONLY)
    async with client_for(raw) as client:
        out = (
            await client.call_tool(
                "diff_device_facts",
                {
                    "device_id": str(device.id),
                    "from_ts": iso(t0 + timedelta(hours=1)),
                    "to_ts": iso(t1 + timedelta(hours=1)),
                },
            )
        ).data
    assert out["counts"] == {"added": 1, "removed": 0, "changed": 1}
    assert out["added"][0]["fact_key"] == "pkg:jq"
    assert out["changed"][0]["before"] == {"version": "1.1"}
    assert out["changed"][0]["after"] == {"version": "3.0"}


# --- mutating tools refuse to act without confirm -------------------------


async def test_run_script_dry_run_queues_nothing():
    first = await add_device("prod-01", tags=["prod"])
    await add_device("prod-02", tags=["prod"])
    await add_device("dev-01", tags=["dev"])
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            Script(
                name="disk-cleanup",
                description="Free space in /tmp",
                shell=ShellKind.bash,
                body="rm -rf /tmp/*",
                sha256="a" * 64,
            )
        )

    raw = await mint_key("operator", FULL)
    async with client_for(raw) as client:
        by_tag = (
            await client.call_tool("run_script", {"script_name": "disk-cleanup", "tags": ["prod"]})
        ).data
        assert by_tag["dry_run"] is True
        assert by_tag["queued"] == 0
        assert by_tag["device_count"] == 2
        assert sorted(by_tag["would_run_on"]) == ["prod-01", "prod-02"]
        assert by_tag["script"]["shell"] == "bash"

        by_id = (
            await client.call_tool(
                "run_script", {"script_name": "disk-cleanup", "device_ids": [str(first.id)]}
            )
        ).data
        assert by_id["would_run_on"] == ["prod-01"]

        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "run_script", {"script_name": "no-such-script", "tags": ["prod"]}
            )
        assert "disk-cleanup" in str(exc.value)  # suggests what does exist

        with pytest.raises(ToolError) as exc:
            await client.call_tool("run_script", {"script_name": "disk-cleanup"})
        assert "no fleet-wide default" in str(exc.value)

        with pytest.raises(ToolError) as exc:
            await client.call_tool(
                "run_script",
                {
                    "script_name": "disk-cleanup",
                    "tags": ["prod"],
                    "device_ids": [str(first.id)],
                },
            )
        assert "not both" in str(exc.value)

    # the whole point: a dry run is not a run
    assert await count(ScriptRun) == 0
    assert await count(ScriptRun, ScriptRun.status == RunStatus.queued) == 0

    rows = await audit_rows("mcp.run_script")
    assert len(rows) == 5
    assert [r.detail["ok"] for r in rows] == [True, True, False, False, False]
    assert all(r.detail["dry_run"] is True for r in rows)


async def test_approve_patches_dry_run_approves_nothing():
    device = await add_device("patch-01", os_family=OsFamily.windows)
    raw = await mint_key("operator", FULL)
    async with client_for(raw) as client:
        out = (
            await client.call_tool(
                "approve_patches",
                {"device_id": str(device.id), "external_ids": ["KB5000001"]},
            )
        ).data
    assert out["dry_run"] is True
    assert out["approved"] == 0
    assert out["not_in_catalog"] == ["KB5000001"]
    assert await count(PatchApproval) == 0


async def test_pending_patches_then_approval():
    device = await add_device("patch-02", os_family=OsFamily.windows)
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            PatchCatalog(
                os_family=OsFamily.windows,
                external_id="KB5000123",
                title="Cumulative update",
                severity=PatchSeverity.critical,
            )
        )
        await record_facts(
            db,
            "patchstate",
            device.id,
            {"patch:KB5000123": {"title": "Cumulative update", "severity": "critical"}},
        )

    raw = await mint_key("operator", FULL)
    async with client_for(raw) as client:
        pending = (
            await client.call_tool("list_pending_patches", {"device_id": str(device.id)})
        ).data
        assert pending["count"] == 1
        assert pending["patches"][0]["title"] == "Cumulative update"
        assert pending["patches"][0]["approved"] is False

        preview = (
            await client.call_tool(
                "approve_patches",
                {"device_id": str(device.id), "external_ids": ["KB5000123"]},
            )
        ).data
        assert preview["would_approve"][0]["severity"] == "critical"
        assert await count(PatchApproval) == 0

        done = (
            await client.call_tool(
                "approve_patches",
                {
                    "device_id": str(device.id),
                    "external_ids": ["KB5000123"],
                    "confirm": True,
                },
            )
        ).data
        assert done["dry_run"] is False
        assert done["approved"] == 1
        assert done["decided_by"] == "mcp:operator"

        after = (await client.call_tool("list_pending_patches", {"device_id": str(device.id)})).data
        assert after["patches"][0]["approved"] is True

    assert await count(PatchApproval) == 1


async def test_alerts_listed_then_acknowledged():
    device = await add_device("alerting-01")
    rule_id = uuid.uuid4()
    alert_id = uuid.uuid4()
    async with get_sessionmaker()() as db, db.begin():
        db.add(
            AlertRule(
                id=rule_id,
                name="cpu high",
                metric=Metric.cpu,
                threshold=90,
                severity=Severity.warning,
            )
        )
        # no relationship() between the two models, so the unit of work cannot
        # infer the insert order for us
        await db.flush()
        db.add(
            Alert(
                id=alert_id,
                rule_id=rule_id,
                device_id=device.id,
                state=AlertState.firing,
                severity=Severity.warning,
                last_value=97.5,
                context={"rule": "cpu high", "metric": "cpu", "hostname": device.hostname},
            )
        )

    raw = await mint_key("operator", FULL)
    async with client_for(raw) as client:
        firing = (await client.call_tool("list_alerts", {})).data
        assert len(firing) == 1
        assert firing[0]["rule"] == "cpu high"
        assert firing[0]["hostname"] == "alerting-01"
        assert firing[0]["last_value"] == 97.5

        assert (await client.call_tool("list_alerts", {"state": "resolved"})).data == []

        preview = (await client.call_tool("acknowledge_alert", {"alert_id": str(alert_id)})).data
        assert preview["dry_run"] is True
        assert preview["acknowledged"] is False
        assert preview["would_acknowledge"]["hostname"] == "alerting-01"

        async with get_sessionmaker()() as db:
            unchanged = (await db.execute(select(Alert).where(Alert.id == alert_id))).scalar_one()
            assert unchanged.state is AlertState.firing

        done = (
            await client.call_tool(
                "acknowledge_alert",
                {"alert_id": str(alert_id), "note": "maintenance window", "confirm": True},
            )
        ).data
        assert done["acknowledged"] is True
        assert done["acked_by"] == "mcp:operator"

    async with get_sessionmaker()() as db:
        acked = (await db.execute(select(Alert).where(Alert.id == alert_id))).scalar_one()
    assert acked.state is AlertState.acknowledged
    assert acked.acked_by == "mcp:operator"

    rows = await audit_rows("mcp.acknowledge_alert")
    assert [r.detail["dry_run"] for r in rows] == [True, False]
    assert rows[1].detail["note"] == "maintenance window"
    assert rows[1].target_id == str(alert_id)


# --- annotation contract -------------------------------------------------
#
# Clients decide whether to auto-approve a call from these hints. A tool that
# claims readOnlyHint while mutating, or drops destructiveHint from run_script,
# would let an assistant execute code on endpoints without ever prompting.

EXPECTED_ANNOTATIONS = {
    "list_devices": {"readOnlyHint": True, "destructiveHint": False},
    "get_device": {"readOnlyHint": True, "destructiveHint": False},
    "get_device_facts": {"readOnlyHint": True, "destructiveHint": False},
    "diff_device_facts": {"readOnlyHint": True, "destructiveHint": False},
    "list_alerts": {"readOnlyHint": True, "destructiveHint": False},
    "list_pending_patches": {"readOnlyHint": True, "destructiveHint": False},
    "acknowledge_alert": {"readOnlyHint": False, "destructiveHint": False, "idempotentHint": True},
    "run_script": {"readOnlyHint": False, "destructiveHint": True, "idempotentHint": False},
    "approve_patches": {"readOnlyHint": False, "destructiveHint": False, "idempotentHint": True},
}


async def test_every_tool_is_annotated_and_titled():
    from fastmcp import Client

    from everwas.mcp.server import mcp

    async with Client(mcp) as client:
        tools = {t.name: t for t in await client.list_tools()}

    assert set(tools) == set(EXPECTED_ANNOTATIONS), "tool set changed; update the contract"

    for name, expected in EXPECTED_ANNOTATIONS.items():
        tool = tools[name]
        assert tool.description, f"{name} has no description"
        assert tool.title, f"{name} has no human-readable title"
        assert tool.annotations is not None, f"{name} has no annotations"
        actual = tool.annotations.model_dump(exclude_none=True)
        for hint, value in expected.items():
            assert actual.get(hint) == value, f"{name}.{hint} is {actual.get(hint)}, want {value}"
        # The fleet is an enumerable set of machines, never an open world.
        assert actual.get("openWorldHint") is False, f"{name} claims open world"


async def test_mutating_tools_all_require_confirmation():
    """Every non-read-only tool must expose `confirm`, defaulting to false."""
    from fastmcp import Client

    from everwas.mcp.server import mcp

    async with Client(mcp) as client:
        tools = await client.list_tools()

    for tool in tools:
        if tool.annotations and tool.annotations.readOnlyHint:
            continue
        props = (tool.inputSchema or {}).get("properties", {})
        assert "confirm" in props, f"{tool.name} mutates without a confirm parameter"
        assert props["confirm"].get("default") is False, f"{tool.name} confirm must default false"
