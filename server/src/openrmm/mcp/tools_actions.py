"""Mutating tools. Every one of them refuses to act until `confirm=true`.

The dry run is not a formality: it is the plan a human reads before anything
touches a machine. Tools return the same shape either way, with `dry_run`
saying which happened.
"""

from datetime import UTC, datetime
from typing import Annotated

from fastmcp import FastMCP
from fastmcp.exceptions import ToolError
from pydantic import Field
from sqlalchemy import or_, select

from openrmm.db.engine import session_scope
from openrmm.mcp.context import (
    get_nats_connection,
    iso,
    parse_uuid,
    tool_call,
)
from openrmm.models.alert import Alert, AlertRule, AlertState
from openrmm.models.device import Device
from openrmm.models.patch import ApprovalDecision, PatchApproval, PatchCatalog
from openrmm.models.script import RunTrigger, Script
from openrmm.services.jobs import queue_script_run, resolve_targets
from openrmm.services.patching import approve

MAX_TARGETS = 500
MAX_PATCH_IDS = 200
CONFIRM_HINT = "Show this plan to the user, then call again with confirm=true to carry it out."


async def acknowledge_alert(
    alert_id: Annotated[str, Field(description="Alert UUID from list_alerts.")],
    note: Annotated[
        str, Field(description="Why it is being acknowledged. Recorded in the audit log.")
    ] = "",
    confirm: Annotated[
        bool, Field(description="Must be true to actually acknowledge. False previews.")
    ] = False,
) -> dict:
    """Acknowledge a firing alert, silencing it without pretending it is fixed.

    With `confirm=false` (the default) this only describes the alert it would
    acknowledge, so the user can check you picked the right one.
    """
    async with tool_call("mcp.acknowledge_alert", "alerts:write", target_type="alert") as call:
        alert_uuid = parse_uuid(alert_id, "alert_id")
        call.target_id = str(alert_uuid)
        call.detail["dry_run"] = not confirm
        if note:
            call.detail["note"] = note[:500]

        async with session_scope() as db:
            row = (
                await db.execute(
                    select(Alert, AlertRule.name, Device.hostname)
                    .join(AlertRule, AlertRule.id == Alert.rule_id)
                    .join(Device, Device.id == Alert.device_id)
                    .where(Alert.id == alert_uuid)
                )
            ).one_or_none()
            if row is None:
                raise ToolError(
                    f"No alert with id {alert_uuid}. Use list_alerts to find current alerts."
                )
            alert, rule_name, hostname = row

            subject = {
                "alert_id": str(alert.id),
                "rule": rule_name,
                "severity": alert.severity.value,
                "state": alert.state.value,
                "device_id": str(alert.device_id),
                "hostname": hostname,
                "opened_at": iso(alert.opened_at),
            }

            if alert.state is AlertState.resolved:
                raise ToolError(
                    f"Alert {alert_uuid} is already resolved, so there is nothing to "
                    "acknowledge. It fired on "
                    f"{hostname} and cleared at {iso(alert.resolved_at)}."
                )
            if alert.state is AlertState.acknowledged:
                call.detail["already_acknowledged"] = True
                return {
                    "dry_run": not confirm,
                    "acknowledged": False,
                    "reason": "already acknowledged",
                    "alert": {
                        **subject,
                        "acked_by": alert.acked_by,
                        "acked_at": iso(alert.acked_at),
                    },
                }

            if not confirm:
                return {
                    "dry_run": True,
                    "acknowledged": False,
                    "would_acknowledge": subject,
                    "confirm_hint": CONFIRM_HINT,
                }

            alert.state = AlertState.acknowledged
            alert.acked_at = datetime.now(UTC)
            alert.acked_by = call.actor
            acked_by = alert.acked_by
            acked_at = iso(alert.acked_at)

        return {
            "dry_run": False,
            "acknowledged": True,
            "alert": {**subject, "state": AlertState.acknowledged.value},
            "acked_by": acked_by,
            "acked_at": acked_at,
            "note": note or None,
        }


async def run_script(
    script_name: Annotated[
        str, Field(description="Exact name of a script in the OpenRMM library.")
    ],
    device_ids: Annotated[
        list[str] | None, Field(description="Device UUIDs to run on. Use this or tags, not both.")
    ] = None,
    tags: Annotated[
        list[str] | None, Field(description="Run on every device carrying any of these tags.")
    ] = None,
    confirm: Annotated[
        bool, Field(description="Must be true to queue the run. False returns the plan only.")
    ] = False,
) -> dict:
    """Queue a stored script to run on selected machines.

    Scripts are referenced by name, the same name a human sees in the OpenRMM
    library. Targets come from explicit device ids or from tags, never from
    both at once, and never implicitly from the whole fleet.

    With `confirm=false` (the default) nothing is queued: you get the script
    detail and the exact list of hostnames it would run on. Show that list to
    the user before confirming, because a script that runs is a script that
    already ran.
    """
    async with tool_call("mcp.run_script", "scripts:run", target_type="script") as call:
        call.detail.update({"dry_run": not confirm, "script_name": script_name})
        device_ids = [d for d in (device_ids or []) if str(d).strip()]
        tags = [t for t in (tags or []) if str(t).strip()]

        if device_ids and tags:
            raise ToolError(
                "Pass device_ids or tags, not both. Targeting is deliberately one "
                "selector so the blast radius is obvious."
            )
        if not device_ids and not tags:
            raise ToolError(
                "Name the targets: pass device_ids (from list_devices) or tags. "
                "There is no fleet-wide default, on purpose."
            )
        nc = get_nats_connection() if confirm else None

        async with session_scope() as db:
            script = (
                await db.execute(select(Script).where(Script.name == script_name))
            ).scalar_one_or_none()
            if script is None:
                known = list(
                    (
                        await db.execute(select(Script.name).order_by(Script.name).limit(25))
                    ).scalars()
                )
                available = ", ".join(known) if known else "none defined yet"
                raise ToolError(
                    f"No script named {script_name!r}. Scripts are matched by exact name. "
                    f"Available: {available}."
                )
            call.target_id = str(script.id)

            # Tags are resolved to ids first so the plan can name every host
            # before anything is queued. resolve_targets still applies the
            # script's os_filter and drops retired devices.
            if tags:
                matched = (
                    await db.execute(
                        select(Device.id).where(or_(*[Device.tags.any(t) for t in tags]))
                    )
                ).scalars()
                target = {"device_ids": [str(device_id) for device_id in matched]}
            else:
                target = {"device_ids": [str(parse_uuid(d)) for d in device_ids]}

            devices = await resolve_targets(db, target, script)
            if len(devices) > MAX_TARGETS:
                raise ToolError(
                    f"That selector matches {len(devices)} devices, above the {MAX_TARGETS} "
                    "device ceiling for one MCP run. Narrow it, or use the OpenRMM UI."
                )

            plan = {
                "script": {
                    "name": script.name,
                    "description": script.description,
                    "shell": script.shell.value,
                    "timeout_s": script.timeout_s,
                    "os_filter": list(script.os_filter or []),
                },
                "would_run_on": [d.hostname for d in devices],
                "device_count": len(devices),
            }
            call.detail["device_count"] = len(devices)

            if not confirm:
                return {
                    **plan,
                    "dry_run": True,
                    "queued": 0,
                    "confirm_hint": CONFIRM_HINT,
                    "note": (
                        "Nothing has been queued. Devices filtered out by the script's "
                        "os_filter never appear in would_run_on."
                    ),
                }
            if not devices:
                raise ToolError(
                    "That selector matched no devices, so there is nothing to run. "
                    "Check the tag spelling or the device ids with list_devices."
                )

            requested_by = call.actor
            batch_id, runs = await queue_script_run(
                db,
                nc,
                script,
                devices,
                requested_by=requested_by,
                trigger=RunTrigger.mcp,
            )
            queued = [
                {"run_id": str(run.id), "device_id": str(device.id), "hostname": device.hostname}
                for run, device in zip(runs, devices, strict=True)
            ]

        call.detail["batch_id"] = str(batch_id)
        return {
            **plan,
            "dry_run": False,
            "queued": len(queued),
            "batch_id": str(batch_id),
            "runs": queued,
            "requested_by": requested_by,
            "note": "Jobs are durable: an offline machine runs its job when it reconnects.",
        }


async def approve_patches(
    device_id: Annotated[str, Field(description="Device UUID from list_devices.")],
    external_ids: Annotated[
        list[str],
        Field(description="Patch external_ids exactly as list_pending_patches reports them."),
    ],
    confirm: Annotated[
        bool, Field(description="Must be true to record the approvals. False previews.")
    ] = False,
) -> dict:
    """Approve specific patches for one machine. This does not install anything.

    Approval and installation are separate steps on purpose: approving says
    "this update is allowed here", and a patch job or policy is what later
    deploys it during a maintenance window.

    With `confirm=false` (the default) you get the list of patches that would
    be approved, which ones already are, and which ids the catalog does not
    recognize for this machine's OS family.
    """
    async with tool_call("mcp.approve_patches", "patches:write", target_type="device") as call:
        device_uuid = parse_uuid(device_id)
        call.target_id = str(device_uuid)
        call.detail["dry_run"] = not confirm

        wanted = [str(e).strip() for e in (external_ids or []) if str(e).strip()]
        if not wanted:
            raise ToolError(
                "external_ids is empty. Pass the patch ids from list_pending_patches; "
                "there is no approve-everything shortcut."
            )
        if len(wanted) > MAX_PATCH_IDS:
            raise ToolError(
                f"{len(wanted)} patch ids is above the {MAX_PATCH_IDS} ceiling for one call. "
                "Approve them in smaller, reviewable batches."
            )

        async with session_scope() as db:
            device = (
                await db.execute(select(Device).where(Device.id == device_uuid))
            ).scalar_one_or_none()
            if device is None:
                raise ToolError(f"No device with id {device_uuid}. Use list_devices to find it.")

            catalog = {
                row.external_id: row
                for row in (
                    await db.execute(
                        select(PatchCatalog).where(
                            PatchCatalog.os_family == device.os_family,
                            PatchCatalog.external_id.in_(wanted),
                        )
                    )
                ).scalars()
            }
            unknown = [e for e in wanted if e not in catalog]

            already = set(
                (
                    await db.execute(
                        select(PatchCatalog.external_id)
                        .join(PatchApproval, PatchApproval.patch_id == PatchCatalog.id)
                        .where(
                            PatchCatalog.os_family == device.os_family,
                            PatchCatalog.external_id.in_(list(catalog)),
                            PatchApproval.decision == ApprovalDecision.approved,
                            (PatchApproval.device_id == device.id)
                            | (PatchApproval.device_id.is_(None)),
                        )
                    )
                ).scalars()
            )

            def described(ext: str) -> dict:
                entry = catalog[ext]
                return {
                    "external_id": ext,
                    "title": entry.title or ext,
                    "severity": entry.severity.value,
                    "reboot_likely": bool(entry.reboot_likely),
                }

            pending = [described(e) for e in wanted if e in catalog and e not in already]
            subject = {
                "device": {
                    "id": str(device.id),
                    "hostname": device.hostname,
                    "os_family": device.os_family.value,
                },
                "would_approve": pending,
                "already_approved": sorted(already),
                "not_in_catalog": unknown,
            }
            call.detail.update(
                {"requested": len(wanted), "pending": len(pending), "unknown": len(unknown)}
            )

            if not confirm:
                return {
                    **subject,
                    "dry_run": True,
                    "approved": 0,
                    "confirm_hint": CONFIRM_HINT,
                    "note": "Approving does not install anything by itself.",
                }
            if not pending:
                if unknown and not already:
                    raise ToolError(
                        f"None of those ids are in the patch catalog for "
                        f"{device.os_family.value}: {', '.join(unknown[:10])}. "
                        "Use list_pending_patches to get exact external_ids."
                    )
                return {
                    **subject,
                    "dry_run": False,
                    "approved": 0,
                    "note": "Nothing to do: those patches were already approved.",
                }

            decided_by = call.actor
            for entry in pending:
                await approve(
                    db,
                    catalog[entry["external_id"]].id,
                    device_id=device.id,
                    decision=ApprovalDecision.approved,
                    decided_by=decided_by,
                )

        return {
            **subject,
            "dry_run": False,
            "approved": len(pending),
            "decided_by": decided_by,
            "note": (
                "Approved only. Deployment happens through a patch job or a patch "
                "policy's maintenance window."
            ),
        }


def register(mcp: FastMCP) -> None:
    mutating = {"readOnlyHint": False, "openWorldHint": False}

    def _write(fn, title: str, tags: set[str], **hints) -> None:
        mcp.tool(fn, title=title, tags=tags, annotations={**mutating, "title": title, **hints})

    # Acknowledging twice leaves the same alert acknowledged.
    _write(
        acknowledge_alert,
        "Acknowledge an alert",
        {"alerts", "write"},
        destructiveHint=False,
        idempotentHint=True,
    )
    # A script can do anything on the endpoint, and running it twice runs it
    # twice. This is the one tool a client should always prompt for.
    _write(
        run_script,
        "Run a script on devices",
        {"scripts", "write"},
        destructiveHint=True,
        idempotentHint=False,
    )
    # Approving is an upsert on (patch, device): re-approving changes nothing,
    # and approving still installs nothing.
    _write(
        approve_patches,
        "Approve patches for installation",
        {"patches", "write"},
        destructiveHint=False,
        idempotentHint=True,
    )
