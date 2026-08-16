"""Read-only fleet tools: inventory, the bitemporal time machine, alerts, patches."""

import uuid
from typing import Annotated

from fastmcp import FastMCP
from fastmcp.exceptions import ToolError
from pydantic import Field
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.bitemporal.query import diff_facts, get_facts
from openrmm.db.engine import session_scope
from openrmm.mcp.context import (
    iso,
    like_escape,
    parse_choice,
    parse_ts,
    parse_uuid,
    require_ts,
    tool_call,
)
from openrmm.models.alert import Alert, AlertRule, AlertState
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.facts import FACT_TABLES
from openrmm.models.patch import PatchCatalog
from openrmm.models.telemetry import DeviceStatusLatest
from openrmm.services.patching import approved_external_ids

# Context-window guards. A laptop can report 2000 packages; nobody needs all of
# them in a chat transcript, and the truncated flag tells the assistant to narrow.
DEVICE_CAP = 500
FACT_CAP = 500
DIFF_CAP = 200
PATCH_CAP = 500

FACT_KINDS = tuple(sorted(FACT_TABLES))
DEVICE_STATUSES = tuple(s.value for s in DeviceStatus)
ALERT_STATES = tuple(s.value for s in AlertState)


def _summary(device: Device, latest: DeviceStatusLatest | None) -> dict:
    return {
        "id": str(device.id),
        "hostname": device.hostname,
        "os_family": device.os_family.value,
        "os_version": device.os_version,
        "status": device.status.value,
        "last_heartbeat_at": iso(device.last_heartbeat_at),
        "tags": list(device.tags or []),
        "cpu_pct": latest.cpu_pct if latest else None,
        "mem_pct": latest.mem_pct if latest else None,
        "worst_disk_pct": latest.worst_disk_pct if latest else None,
        "telemetry_at": iso(latest.ts) if latest else None,
    }


async def _device_or_error(db: AsyncSession, device_id: uuid.UUID) -> Device:
    device = (await db.execute(select(Device).where(Device.id == device_id))).scalar_one_or_none()
    if device is None:
        raise ToolError(
            f"No device with id {device_id}. Use list_devices to find the right one; "
            "device ids are stable, hostnames are not."
        )
    return device


async def list_devices(
    status: Annotated[
        str | None,
        Field(description="Filter by lifecycle state: enrolled, active, offline, or retired."),
    ] = None,
    tag: Annotated[str | None, Field(description="Only devices carrying this exact tag.")] = None,
    hostname_contains: Annotated[
        str | None, Field(description="Case-insensitive hostname substring, e.g. 'web'.")
    ] = None,
) -> list[dict]:
    """List machines in the fleet with their current health snapshot.

    Start here when the user names a machine by hostname rather than id. Each
    entry carries the device id the other tools need, plus the most recent
    CPU / memory / worst-disk percentages when the agent has reported telemetry.

    Returns at most 500 devices; use the filters to narrow a large fleet.
    """
    async with tool_call("mcp.list_devices", "devices:read") as call:
        wanted = parse_choice(status, DEVICE_STATUSES, "status") if status else None

        async with session_scope() as db:
            stmt = (
                select(Device, DeviceStatusLatest)
                .join(
                    DeviceStatusLatest,
                    DeviceStatusLatest.device_id == Device.id,
                    isouter=True,
                )
                .order_by(Device.hostname)
                .limit(DEVICE_CAP)
            )
            if wanted:
                stmt = stmt.where(Device.status == DeviceStatus(wanted))
            if tag:
                # `any()`, not `contains()`: Device.tags is the generic ARRAY type,
                # whose comparator has no PostgreSQL @> operator.
                stmt = stmt.where(Device.tags.any(tag))
            if hostname_contains:
                stmt = stmt.where(
                    Device.hostname.ilike(f"%{like_escape(hostname_contains)}%", escape="\\")
                )
            rows = (await db.execute(stmt)).all()

        out = [_summary(device, latest) for device, latest in rows]
        call.detail.update(
            {
                "count": len(out),
                "status": wanted,
                "tag": tag,
                "hostname_contains": hostname_contains,
            }
        )
        return out


async def get_device(
    device_id: Annotated[str, Field(description="Device UUID from list_devices.")],
) -> dict:
    """Full detail for one machine: identity, agent build, and latest telemetry.

    Use this before acting on a device so you can tell the user which machine
    you mean and whether the agent is currently reporting.
    """
    async with tool_call("mcp.get_device", "devices:read", target_type="device") as call:
        device_uuid = parse_uuid(device_id)
        call.target_id = str(device_uuid)

        async with session_scope() as db:
            device = await _device_or_error(db, device_uuid)
            latest = (
                await db.execute(
                    select(DeviceStatusLatest).where(DeviceStatusLatest.device_id == device_uuid)
                )
            ).scalar_one_or_none()
            out = _summary(device, latest)
            out.update(
                {
                    "arch": device.arch,
                    "agent_version": device.agent_version,
                    "enrolled_at": iso(device.enrolled_at),
                    "site_id": str(device.site_id) if device.site_id else None,
                }
            )

        call.detail["hostname"] = device.hostname
        return out


async def get_device_facts(
    device_id: Annotated[str, Field(description="Device UUID from list_devices.")],
    kind: Annotated[
        str, Field(description="Which fact set: software, hardware, or patchstate.")
    ] = "software",
    as_of: Annotated[
        str | None,
        Field(
            description=(
                "VALID TIME. What was true on the machine at this ISO-8601 instant "
                "(e.g. '2026-08-01T13:00:00Z'). Omit for right now."
            )
        ),
    ] = None,
    knew_at: Annotated[
        str | None,
        Field(
            description=(
                "RECORD TIME. Answer using only what the server believed at this "
                "ISO-8601 instant. Omit for our current knowledge."
            )
        ),
    ] = None,
) -> dict:
    """Inspect a machine's inventory at any point on either time axis.

    This is the time machine, and the two axes answer different questions:

    - `as_of` is valid time: "what was installed on web-01 last Tuesday?"
    - `knew_at` is record time: "what did we *believe* was installed on web-01,
      going on the reports we had received by last Tuesday?"

    They diverge whenever an agent reports late or a later scan corrects an
    earlier belief. After an incident, `knew_at` is what tells you whether
    monitoring knew about a vulnerable package at the time or only learned
    later. Combine them freely: as_of last Monday with knew_at last Tuesday is
    "what we thought on Tuesday had been true on Monday".

    Timestamps must carry a timezone. Facts are capped at 500 per call.
    """
    async with tool_call("mcp.get_device_facts", "devices:read", target_type="device") as call:
        device_uuid = parse_uuid(device_id)
        call.target_id = str(device_uuid)
        fact_kind = parse_choice(kind, FACT_KINDS, "kind")
        as_of_dt = parse_ts(as_of, "as_of")
        knew_at_dt = parse_ts(knew_at, "knew_at")

        async with session_scope() as db:
            device = await _device_or_error(db, device_uuid)
            facts = await get_facts(db, fact_kind, device_uuid, as_of=as_of_dt, knew_at=knew_at_dt)

        shown = facts[:FACT_CAP]
        call.detail.update(
            {
                "kind": fact_kind,
                "as_of": iso(as_of_dt),
                "knew_at": iso(knew_at_dt),
                "count": len(facts),
            }
        )
        return {
            "device": {"id": str(device.id), "hostname": device.hostname},
            "kind": fact_kind,
            "as_of": iso(as_of_dt),
            "knew_at": iso(knew_at_dt),
            "count": len(facts),
            "truncated": len(facts) > len(shown),
            "facts": [
                {
                    "fact_key": f["fact_key"],
                    "payload": f["payload"],
                    "valid_from": iso(f["valid_from"]),
                    "valid_to": iso(f["valid_to"]),
                    "source": f["source"],
                }
                for f in shown
            ],
        }


async def diff_device_facts(
    device_id: Annotated[str, Field(description="Device UUID from list_devices.")],
    from_ts: Annotated[
        str, Field(description="Earlier instant in VALID time, ISO-8601 with timezone.")
    ],
    to_ts: Annotated[
        str, Field(description="Later instant in VALID time, ISO-8601 with timezone.")
    ],
    kind: Annotated[
        str, Field(description="Which fact set: software, hardware, or patchstate.")
    ] = "software",
    knew_at: Annotated[
        str | None,
        Field(description="Answer using only what the server believed at this instant."),
    ] = None,
) -> dict:
    """What changed on a machine between two moments, on either time axis.

    Answers "what did this laptop gain, lose, or upgrade last week" using
    today's knowledge, or, with `knew_at`, using only what the server believed
    at that earlier moment. Added / removed / changed lists are capped at 200
    entries each; the counts are exact regardless.
    """
    async with tool_call("mcp.diff_device_facts", "devices:read", target_type="device") as call:
        device_uuid = parse_uuid(device_id)
        call.target_id = str(device_uuid)
        fact_kind = parse_choice(kind, FACT_KINDS, "kind")
        start = require_ts(from_ts, "from_ts")
        end = require_ts(to_ts, "to_ts")
        if start > end:
            raise ToolError("from_ts is later than to_ts. Pass the earlier instant first.")
        knew_at_dt = parse_ts(knew_at, "knew_at")

        async with session_scope() as db:
            device = await _device_or_error(db, device_uuid)
            delta = await diff_facts(
                db, fact_kind, device_uuid, from_ts=start, to_ts=end, knew_at=knew_at_dt
            )

        counts = {bucket: len(rows) for bucket, rows in delta.items()}
        call.detail.update({"kind": fact_kind, **counts})
        return {
            "device": {"id": str(device.id), "hostname": device.hostname},
            "kind": fact_kind,
            "from_ts": iso(start),
            "to_ts": iso(end),
            "knew_at": iso(knew_at_dt),
            "counts": counts,
            "truncated": any(n > DIFF_CAP for n in counts.values()),
            "added": delta["added"][:DIFF_CAP],
            "removed": delta["removed"][:DIFF_CAP],
            "changed": delta["changed"][:DIFF_CAP],
        }


async def list_alerts(
    state: Annotated[
        str | None,
        Field(description="firing, acknowledged, or resolved. Pass null for every state."),
    ] = "firing",
    device_id: Annotated[str | None, Field(description="Restrict to one device UUID.")] = None,
    limit: Annotated[int, Field(description="Maximum alerts to return, 1 to 200.")] = 50,
) -> list[dict]:
    """Current alerts, newest first, with the rule and hostname that produced them.

    Defaults to what is firing right now, which is usually the question being
    asked. Each entry carries the alert id that acknowledge_alert needs.
    """
    async with tool_call("mcp.list_alerts", "alerts:read") as call:
        wanted = parse_choice(state, ALERT_STATES, "state") if state else None
        device_uuid = parse_uuid(device_id) if device_id else None
        capped = max(1, min(int(limit), 200))

        async with session_scope() as db:
            stmt = (
                select(Alert, AlertRule.name, Device.hostname)
                .join(AlertRule, AlertRule.id == Alert.rule_id)
                .join(Device, Device.id == Alert.device_id)
                .order_by(Alert.opened_at.desc())
                .limit(capped)
            )
            if wanted:
                stmt = stmt.where(Alert.state == AlertState(wanted))
            if device_uuid is not None:
                stmt = stmt.where(Alert.device_id == device_uuid)
            rows = (await db.execute(stmt)).all()

        out = []
        for alert, rule_name, hostname in rows:
            context = alert.context or {}
            out.append(
                {
                    "id": str(alert.id),
                    "rule": rule_name or context.get("rule"),
                    "metric": context.get("metric"),
                    "threshold": context.get("threshold"),
                    "severity": alert.severity.value,
                    "state": alert.state.value,
                    "device_id": str(alert.device_id),
                    "hostname": hostname,
                    "last_value": float(alert.last_value) if alert.last_value is not None else None,
                    "opened_at": iso(alert.opened_at),
                    "acked_at": iso(alert.acked_at),
                    "acked_by": alert.acked_by,
                    "resolved_at": iso(alert.resolved_at),
                }
            )
        call.detail.update({"count": len(out), "state": wanted})
        return out


async def list_pending_patches(
    device_id: Annotated[str, Field(description="Device UUID from list_devices.")],
) -> dict:
    """Updates a machine currently reports as pending, with approval state.

    `approved: false` means the patch is known but nobody has signed off on it,
    so it will not install. Approving is a separate, explicit step
    (approve_patches), and approving still does not deploy anything.
    """
    async with tool_call("mcp.list_pending_patches", "patches:read", target_type="device") as call:
        device_uuid = parse_uuid(device_id)
        call.target_id = str(device_uuid)

        async with session_scope() as db:
            device = await _device_or_error(db, device_uuid)
            facts = await get_facts(db, "patchstate", device_uuid)
            external_ids = [f["fact_key"].removeprefix("patch:") for f in facts]

            catalog: dict[str, PatchCatalog] = {}
            approved: set[str] = set()
            if external_ids:
                catalog = {
                    row.external_id: row
                    for row in (
                        await db.execute(
                            select(PatchCatalog).where(
                                PatchCatalog.os_family == device.os_family,
                                PatchCatalog.external_id.in_(external_ids),
                            )
                        )
                    ).scalars()
                }
                approved = set(await approved_external_ids(db, device, external_ids))

        patches = []
        for fact in facts[:PATCH_CAP]:
            ext = fact["fact_key"].removeprefix("patch:")
            payload = fact["payload"] or {}
            entry = catalog.get(ext)
            patches.append(
                {
                    "external_id": ext,
                    "title": (entry.title if entry else payload.get("title", "")) or ext,
                    "kind": entry.kind if entry else payload.get("kind", "other"),
                    "severity": (
                        entry.severity.value if entry else str(payload.get("severity", "unknown"))
                    ),
                    "kb_ids": list(entry.kb_ids) if entry else [],
                    "cves": list(entry.cves) if entry else [],
                    "reboot_likely": bool(
                        entry.reboot_likely if entry else payload.get("reboot_likely")
                    ),
                    "approved": ext in approved,
                    "unsupported": bool(payload.get("unsupported")),
                }
            )

        call.detail.update({"count": len(facts), "approved": len(approved)})
        return {
            "device": {
                "id": str(device.id),
                "hostname": device.hostname,
                "os_family": device.os_family.value,
            },
            "count": len(facts),
            "approved_count": len(approved),
            "truncated": len(facts) > len(patches),
            "patches": patches,
        }


def register(mcp: FastMCP) -> None:
    read_only = {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": False}
    mcp.tool(list_devices, tags={"fleet", "read"}, annotations=read_only)
    mcp.tool(get_device, tags={"fleet", "read"}, annotations=read_only)
    mcp.tool(get_device_facts, tags={"fleet", "read", "bitemporal"}, annotations=read_only)
    mcp.tool(diff_device_facts, tags={"fleet", "read", "bitemporal"}, annotations=read_only)
    mcp.tool(list_alerts, tags={"alerts", "read"}, annotations=read_only)
    mcp.tool(list_pending_patches, tags={"patches", "read"}, annotations=read_only)
