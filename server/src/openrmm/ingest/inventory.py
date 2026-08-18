"""Inventory ingest: routes snapshot kinds to the right store.

- hardware, software, patchstate, network -> bitemporal fact tables
                                   (sequenced amend)
- processes, services            -> device_snapshots (latest-only; too churny
                                    for belief history)
"""

import json
import uuid
from datetime import UTC, datetime

import structlog
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.bitemporal.store import record_facts
from openrmm.ingest.wiretime import WireTimeError, parse_wire_time
from openrmm.models.telemetry import DeviceSnapshot

log = structlog.get_logger()

FACT_KINDS = {"hardware", "software", "patchstate", "network", "logins"}
SNAPSHOT_KINDS = {"processes", "services"}


def parse_inventory(subject: str, payload: bytes) -> tuple[uuid.UUID, str, datetime, dict] | None:
    parts = subject.split(".")
    if len(parts) != 4 or parts[3] not in FACT_KINDS | SNAPSHOT_KINDS:
        return None
    try:
        agent_id = uuid.UUID(parts[1])
        envelope = json.loads(payload)
    except (ValueError, json.JSONDecodeError):
        return None
    if envelope.get("agent_id") != str(agent_id):
        return None
    try:
        # This becomes valid_during. A future-dated fact is invisible to every
        # as_of=now query, so the device would silently report nothing.
        ts = parse_wire_time(envelope.get("ts"))
    except WireTimeError as exc:
        log.warning("rejecting inventory timestamp", agent_id=str(agent_id), reason=str(exc))
        return None
    return agent_id, parts[3], ts, envelope.get("data") or {}


def _facts_from(kind: str, data: dict) -> dict[str, dict]:
    """Flatten a snapshot payload into fact_key -> payload."""
    if kind == "software":
        return {
            f"pkg:{p['name']}": {"version": p.get("version", "")}
            for p in data.get("packages", [])
            if p.get("name")
        }
    if kind == "logins":
        # Keyed on who and where, not on when. A repeat login at the same seat
        # is an amend of the same fact rather than a new one, which keeps the
        # key set bounded by seats instead of growing with every login for the
        # life of the machine.
        return {
            f"login:{login['user']}@{login.get('terminal') or '-'}": login
            for login in (data.get("logins") or [])
            if login.get("user")
        }

    if kind == "posture":
        # Keyed on the check's stable name. Per-check rather than a rollup so
        # that a check added later simply has no history before it existed,
        # and so a single check changing amends only its own belief window.
        return {
            f"check:{c['check']}": {k: v for k, v in c.items() if k != "check"}
            for c in (data.get("checks") or [])
            if c.get("check")
        }

    if kind == "network":
        # One fact per interface, keyed by name. Per-interface rather than one
        # blob for the machine so that a single NIC changing address amends
        # only its own history: a whole-machine fact would rewrite every
        # interface's belief window every time any one of them moved.
        return {f"iface:{i['name']}": i for i in (data.get("interfaces") or []) if i.get("name")}

    if kind == "hardware":
        return {
            "cpu": {"model": data.get("cpu_model", ""), "cores": data.get("cpu_cores")},
            "memory": {"total": data.get("mem_total")},
            "os": {
                "family": data.get("os_family", ""),
                "version": data.get("os_version", ""),
                "kernel": data.get("kernel", ""),
            },
            "system": {
                "hostname": data.get("hostname", ""),
                "arch": data.get("arch", ""),
                "virtualization": data.get("virtualization", ""),
            },
        }
    if kind == "patchstate":
        return {
            f"patch:{p['id']}": {k: v for k, v in p.items() if k != "id"}
            for p in data.get("patches", [])
            if p.get("id")
        }
    raise ValueError(kind)


async def apply_inventory(
    db: AsyncSession, device_id: uuid.UUID, kind: str, observed_at: datetime, data: dict
) -> None:
    snapshot_hash = data.pop("snapshot_hash", "") if isinstance(data, dict) else ""

    if kind == "patchstate":
        # Also feed the catalog and any auto-approve policies before the facts
        # are recorded, so the UI can resolve titles for what it shows.
        from sqlalchemy import select

        from openrmm.models.device import Device
        from openrmm.services.patching import auto_approve_for_policies, upsert_catalog

        device = (
            await db.execute(select(Device).where(Device.id == device_id))
        ).scalar_one_or_none()
        patches = data.get("patches") or []
        if device is not None and patches:
            await upsert_catalog(db, device.os_family, patches)
            approved = await auto_approve_for_policies(db, device, patches)
            if approved:
                log.info("patches auto-approved", device=device.hostname, count=approved)

    if kind in FACT_KINDS:
        result = await record_facts(
            db, kind, device_id, _facts_from(kind, data), observed_at=observed_at
        )
        if result.wrote:
            log.info(
                "facts amended",
                device_id=str(device_id),
                kind=kind,
                added=result.added,
                changed=result.changed,
                removed=result.removed,
            )
        return

    upsert = pg_insert(DeviceSnapshot.__table__).values(
        device_id=device_id,
        kind=kind,
        payload=data,
        snapshot_hash=snapshot_hash,
        updated_at=datetime.now(UTC),
    )
    await db.execute(
        upsert.on_conflict_do_update(
            index_elements=["device_id", "kind"],
            set_={
                "payload": upsert.excluded.payload,
                "snapshot_hash": upsert.excluded.snapshot_hash,
                "updated_at": upsert.excluded.updated_at,
            },
            # skip identical snapshots (hash provided by the agent)
            where=(DeviceSnapshot.__table__.c.snapshot_hash != upsert.excluded.snapshot_hash),
        )
    )
