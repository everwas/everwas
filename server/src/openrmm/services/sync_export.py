"""Query assembly for /api/v1/sync: fleet-wide sweeps, zero per-device calls.

The consumer this serves (a Nautobot SSoT job) refuses to fetch per device —
at fleet size that is thousands of requests. So every function here answers
for a whole page of devices at once: the device page costs three queries
(devices, their hardware facts, their network facts) regardless of page size,
and the fact sweeps are single queries walked by keyset.

Keyset choices:
- devices: ORDER BY id. Device ids are UUIDv7, so this is also enrollment
  order — a free, stable total order.
- fact sweeps: ORDER BY (device_id, fact_key), which is unique under the
  current-belief predicate pair. The bigint fact id would NOT be a stable
  order: sequenced amends insert replacement rows, so an amended fact would
  jump to the end of an id-ordered walk or vanish from it entirely.

Every query carries both bitemporal predicates. upper_inf(recorded_during)
alone includes correction rows (the belief that ENDED is still a belief we
hold about the past); see the long comment in api/v1/devices.py:get_network.
"""

import uuid
from datetime import UTC, datetime, timedelta

from sqlalchemy import Select, func, literal, select, tuple_, union_all
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.config import get_settings
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.facts import (
    FACT_TABLES,
    FactHardware,
    FactNetwork,
    FactPatchState,
    FactPosture,
    FactSoftware,
)
from openrmm.models.org import Organization
from openrmm.models.patch import PatchApproval, PatchCatalog
from openrmm.schemas.sync import (
    SyncChangeOut,
    SyncDeviceOut,
    SyncInterfaceOut,
    SyncOrgOut,
    SyncPatchOut,
    SyncPostureOut,
    SyncSiteOut,
    SyncSoftwareOut,
)
from openrmm.security.api_keys import ApiKeyPrincipal
from openrmm.security.tenancy import scope_to_org

_HARDWARE_KEYS = ("cpu", "memory", "os", "system", "identity")


def _opt(value) -> str | None:
    """'' -> None at the contract boundary: consumers diff these fields, and
    an empty string would read as an assertion where none was made."""
    return value if value else None


def _temporal(model, as_of: datetime | None, knew_at: datetime | None) -> list:
    conditions = []
    if knew_at is None:
        conditions.append(func.upper_inf(model.recorded_during))
    else:
        conditions.append(model.recorded_during.contains(knew_at))
    if as_of is None:
        conditions.append(func.upper_inf(model.valid_during))
    else:
        conditions.append(model.valid_during.contains(as_of))
    return conditions


# --- organizations and sites ------------------------------------------------


async def org_page(db: AsyncSession, principal: ApiKeyPrincipal) -> list[SyncOrgOut]:
    """The caller's organization. An API key belongs to exactly one org, so
    this is a one-element page — the page SHAPE is kept so the consumer
    walks every sync endpoint identically."""
    if principal.org_id is None:
        return []
    org = await db.get(Organization, principal.org_id)
    if org is None:
        return []
    return [SyncOrgOut(id=org.id, name=org.name, description=org.description)]


async def site_page(
    db: AsyncSession,
    principal: ApiKeyPrincipal,
    *,
    cursor_id: uuid.UUID | None,
    limit: int,
) -> tuple[list[SyncSiteOut], bool]:
    from openrmm.models.device import Site

    query = scope_to_org(
        select(Site).order_by(Site.id).limit(limit + 1), Site.org_id, principal.org_id
    )
    if cursor_id is not None:
        query = query.where(Site.id > cursor_id)
    rows = list((await db.execute(query)).scalars())
    has_more = len(rows) > limit
    return [
        SyncSiteOut(
            id=s.id,
            org_id=s.org_id,
            name=s.name,
            description=s.description,
            address=s.address,
            created_at=s.created_at,
        )
        for s in rows[:limit]
    ], has_more


# --- devices ------------------------------------------------------------------


async def device_page(
    db: AsyncSession,
    principal: ApiKeyPrincipal,
    *,
    site_id: uuid.UUID | None,
    cursor_id: uuid.UUID | None,
    limit: int,
) -> tuple[list[SyncDeviceOut], bool]:
    query = scope_to_org(
        select(Device).order_by(Device.id).limit(limit + 1), Device.org_id, principal.org_id
    )
    if site_id is not None:
        query = query.where(Device.site_id == site_id)
    if cursor_id is not None:
        query = query.where(Device.id > cursor_id)
    devices = list((await db.execute(query)).scalars())
    has_more = len(devices) > limit
    devices = devices[:limit]
    if not devices:
        return [], False

    ids = [d.id for d in devices]

    hw: dict[uuid.UUID, dict[str, dict]] = {}
    rows = await db.execute(
        select(FactHardware.device_id, FactHardware.fact_key, FactHardware.payload).where(
            FactHardware.device_id.in_(ids),
            FactHardware.fact_key.in_(_HARDWARE_KEYS),
            *_temporal(FactHardware, None, None),
        )
    )
    for device_id, key, payload in rows:
        hw.setdefault(device_id, {})[key] = payload or {}

    net: dict[uuid.UUID, list[dict]] = {}
    rows = await db.execute(
        select(FactNetwork.device_id, FactNetwork.payload).where(
            FactNetwork.device_id.in_(ids),
            *_temporal(FactNetwork, None, None),
        )
    )
    for device_id, payload in rows:
        net.setdefault(device_id, []).append(payload or {})

    return [_assemble_device(d, hw.get(d.id, {}), net.get(d.id, [])) for d in devices], has_more


def _lifecycle(device: Device) -> str:
    """The durable half of device.status. retired and enrolled-never-
    heartbeated are facts a consumer bases skip policy on; active/offline is
    heartbeat telemetry and collapses to 'operational' here."""
    if device.status == DeviceStatus.retired:
        return "retired"
    if device.status == DeviceStatus.enrolled:
        return "enrolled"
    return "operational"


def _reachable(device: Device) -> bool | None:
    """The volatile half, derived from the heartbeat timestamp rather than
    the status flapper so it cannot disagree with last_heartbeat_at. None
    when the device has never heartbeated — unknown, not unreachable."""
    if device.last_heartbeat_at is None:
        return None
    threshold = timedelta(seconds=get_settings().heartbeat_offline_after_s)
    return datetime.now(UTC) - device.last_heartbeat_at < threshold


def _assemble_device(
    device: Device, facts: dict[str, dict], interfaces: list[dict]
) -> SyncDeviceOut:
    identity = facts.get("identity") or {}
    cpu = facts.get("cpu") or {}
    memory = facts.get("memory") or {}
    system = facts.get("system")

    # Only claim virtual-or-not when a hardware fact exists to base it on.
    is_virtual = bool(system.get("virtualization")) if system is not None else None
    chassis = _opt(identity.get("chassis_type"))
    device_class = "vm" if is_virtual else chassis

    macs: set[str] = set()
    addrs: set[str] = set()
    for iface in interfaces:
        if iface.get("loopback"):
            continue
        if iface.get("mac"):
            macs.add(iface["mac"])
        addrs.update(a for a in (iface.get("addresses") or []) if a)

    return SyncDeviceOut(
        id=device.id,
        org_id=device.org_id,
        site_id=device.site_id,
        hostname=device.hostname,
        status=device.status.value,
        lifecycle=_lifecycle(device),
        reachable=_reachable(device),
        tags=device.tags or [],
        agent_version=device.agent_version,
        os_family=device.os_family.value,
        os_version=device.os_version,
        arch=device.arch,
        enrolled_at=device.enrolled_at,
        last_heartbeat_at=device.last_heartbeat_at,
        manufacturer=_opt(identity.get("manufacturer")),
        model=_opt(identity.get("model")),
        serial_number=_opt(identity.get("serial_number")),
        chassis_type=chassis,
        cpu_model=_opt(cpu.get("model")),
        cpu_cores=cpu.get("cores"),
        memory_bytes=memory.get("total"),
        is_virtual=is_virtual,
        device_class=device_class,
        dns_name=None,
        mac_addresses=sorted(macs),
        ip_addresses=sorted(addrs),
    )


# --- the incremental change feed ------------------------------------------------


async def change_page(
    db: AsyncSession,
    principal: ApiKeyPrincipal,
    *,
    kind: str,
    since: datetime,
    device_id: uuid.UUID | None,
    site_id: uuid.UUID | None,
    cursor: tuple[datetime, int, str] | None,
    limit: int,
) -> tuple[list[SyncChangeOut], bool, tuple | None]:
    """Everything the server learned or unlearned since a moment in record
    time. Two event types off the same rows:

    - recorded: lower(recorded_during) >= since — a belief began (new fact,
      or the new value of an amended one).
    - superseded: upper(recorded_during) >= since — a belief ended (the
      value changed, or the fact disappeared from the machine).

    An incremental consumer replays both in order and lands on current
    state. This is the fleet-wide substitute for calling a per-device diff
    N times, and it is keyed on RECORD time deliberately: late-arriving
    agent reports about the past still show up in the feed, exactly the
    case where a valid-time filter would silently skip them.
    """
    model = FACT_TABLES[kind]

    def _branch(at_expr, change: str, *conditions):
        query = (
            select(
                model.device_id.label("device_id"),
                model.fact_key.label("fact_key"),
                model.payload.label("payload"),
                model.valid_during.label("valid_during"),
                at_expr.label("at"),
                literal(change).label("change"),
                model.id.label("row_id"),
            )
            .join(Device, Device.id == model.device_id)
            .where(*conditions)
        )
        query = scope_to_org(query, Device.org_id, principal.org_id)
        if device_id is not None:
            query = query.where(model.device_id == device_id)
        if site_id is not None:
            query = query.where(Device.site_id == site_id)
        return query

    recorded = _branch(
        func.lower(model.recorded_during), "recorded", func.lower(model.recorded_during) >= since
    )
    superseded = _branch(
        func.upper(model.recorded_during),
        "superseded",
        ~func.upper_inf(model.recorded_during),
        func.upper(model.recorded_during) >= since,
    )

    events = union_all(recorded, superseded).subquery()
    query = select(events).order_by(events.c.at, events.c.row_id, events.c.change).limit(limit + 1)
    if cursor is not None:
        query = query.where(tuple_(events.c.at, events.c.row_id, events.c.change) > cursor)

    rows = (await db.execute(query)).all()
    has_more = len(rows) > limit
    rows = rows[:limit]
    items = [
        SyncChangeOut(
            device_id=r.device_id,
            kind=kind,
            fact_key=r.fact_key,
            payload=r.payload or {},
            change=r.change,
            at=r.at,
            valid_from=r.valid_during.lower,
            valid_to=r.valid_during.upper,
        )
        for r in rows
    ]
    last = (rows[-1].at, rows[-1].row_id, rows[-1].change) if rows and has_more else None
    return items, has_more, last


# --- fleet-wide fact sweeps ---------------------------------------------------


def _fact_sweep(
    model,
    principal: ApiKeyPrincipal,
    *,
    device_id: uuid.UUID | None,
    site_id: uuid.UUID | None,
    cursor: tuple[uuid.UUID, str] | None,
    limit: int,
    as_of: datetime | None,
    knew_at: datetime | None,
) -> Select:
    query = (
        select(model.device_id, model.fact_key, model.payload, model.valid_during)
        .join(Device, Device.id == model.device_id)
        .where(*_temporal(model, as_of, knew_at))
        .order_by(model.device_id, model.fact_key)
        .limit(limit + 1)
    )
    query = scope_to_org(query, Device.org_id, principal.org_id)
    if device_id is not None:
        query = query.where(model.device_id == device_id)
    if site_id is not None:
        query = query.where(Device.site_id == site_id)
    if cursor is not None:
        query = query.where(tuple_(model.device_id, model.fact_key) > cursor)
    return query


async def interface_page(
    db: AsyncSession, principal: ApiKeyPrincipal, **kw
) -> tuple[list[SyncInterfaceOut], bool, tuple | None]:
    rows = (await db.execute(_fact_sweep(FactNetwork, principal, **kw))).all()
    has_more = len(rows) > kw["limit"]
    rows = rows[: kw["limit"]]
    items = [
        SyncInterfaceOut(
            device_id=r.device_id,
            key=r.fact_key.removeprefix("iface:"),
            name=(r.payload or {}).get("name") or r.fact_key.removeprefix("iface:"),
            mac=_opt((r.payload or {}).get("mac")),
            mtu=(r.payload or {}).get("mtu"),
            up=(r.payload or {}).get("up"),
            loopback=bool((r.payload or {}).get("loopback")),
            addresses=[a for a in ((r.payload or {}).get("addresses") or []) if a],
            observed_at=r.valid_during.lower,
        )
        for r in rows
    ]
    last = (rows[-1].device_id, rows[-1].fact_key) if rows and has_more else None
    return items, has_more, last


async def software_page(
    db: AsyncSession, principal: ApiKeyPrincipal, **kw
) -> tuple[list[SyncSoftwareOut], bool, tuple | None]:
    rows = (await db.execute(_fact_sweep(FactSoftware, principal, **kw))).all()
    has_more = len(rows) > kw["limit"]
    rows = rows[: kw["limit"]]
    items = [
        SyncSoftwareOut(
            device_id=r.device_id,
            name=r.fact_key.removeprefix("pkg:"),
            version=str((r.payload or {}).get("version", "")),
            observed_at=r.valid_during.lower,
        )
        for r in rows
    ]
    last = (rows[-1].device_id, rows[-1].fact_key) if rows and has_more else None
    return items, has_more, last


async def posture_page(
    db: AsyncSession, principal: ApiKeyPrincipal, **kw
) -> tuple[list[SyncPostureOut], bool, tuple | None]:
    """One row per (device, security check) — the same sweep as software,
    over fact_posture. The status string passes through untranslated: the
    agent defines the vocabulary, and deciding what an unassessable check
    means belongs to whoever carries the consequence of being wrong."""
    rows = (await db.execute(_fact_sweep(FactPosture, principal, **kw))).all()
    has_more = len(rows) > kw["limit"]
    rows = rows[: kw["limit"]]
    items = [
        SyncPostureOut(
            device_id=r.device_id,
            check=r.fact_key.removeprefix("check:"),
            status=str((r.payload or {}).get("status", "")),
            detail=str((r.payload or {}).get("detail", "")),
            observed_at=r.valid_during.lower,
        )
        for r in rows
    ]
    last = (rows[-1].device_id, rows[-1].fact_key) if rows and has_more else None
    return items, has_more, last


async def patch_page(
    db: AsyncSession, principal: ApiKeyPrincipal, **kw
) -> tuple[list[SyncPatchOut], bool, tuple | None]:
    """Patch state joined to the catalog and the standing approvals.

    The status field is the operator's decision (approved/declined/pending),
    resolved the way deploys resolve it: a device-specific approval shadows a
    fleet-wide one. It is NOT install progress — that lives in patch jobs.
    """
    query = _fact_sweep(FactPatchState, principal, **kw).add_columns(
        Device.os_family.label("os_family")
    )
    rows = (await db.execute(query)).all()
    has_more = len(rows) > kw["limit"]
    rows = rows[: kw["limit"]]
    if not rows:
        return [], False, None

    externals = {r.fact_key.removeprefix("patch:") for r in rows}
    families = {r.os_family for r in rows}
    catalog: dict[tuple, PatchCatalog] = {}
    for c in (
        await db.execute(
            select(PatchCatalog).where(
                PatchCatalog.external_id.in_(externals), PatchCatalog.os_family.in_(families)
            )
        )
    ).scalars():
        catalog[(c.os_family, c.external_id)] = c

    # decisions[(patch_id, device_id)] and decisions[(patch_id, None)]
    decisions: dict[tuple, str] = {}
    if catalog:
        for approval in (
            await db.execute(
                select(PatchApproval).where(
                    PatchApproval.patch_id.in_([c.id for c in catalog.values()])
                )
            )
        ).scalars():
            decisions[(approval.patch_id, approval.device_id)] = approval.decision.value

    items = []
    for r in rows:
        ext = r.fact_key.removeprefix("patch:")
        payload = r.payload or {}
        entry = catalog.get((r.os_family, ext))
        # "pending" is the absence of a decision, deliberately not an
        # ApprovalDecision member: nobody decided it, so nothing stores it.
        if entry is not None:
            status = (
                decisions.get((entry.id, r.device_id))
                or decisions.get((entry.id, None))
                or "pending"
            )
        else:
            status = "pending"
        kb_ids = list(entry.kb_ids) if entry else []
        items.append(
            SyncPatchOut(
                device_id=r.device_id,
                external_id=ext,
                identifier=kb_ids[0] if kb_ids else ext,
                title=(entry.title if entry else payload.get("title", "")) or ext,
                kind=str(entry.kind if entry else payload.get("kind", "other")),
                severity=str(entry.severity if entry else payload.get("severity", "unknown")),
                kb_ids=kb_ids,
                cves=list(entry.cves) if entry else [],
                size_bytes=entry.size_bytes if entry else payload.get("size_bytes"),
                reboot_likely=bool(entry.reboot_likely if entry else payload.get("reboot_likely")),
                status=status,
                unsupported=bool(payload.get("unsupported")),
                detail=str(payload.get("detail", "")),
                observed_at=r.valid_during.lower,
                first_seen_at=entry.first_seen_at if entry else None,
            )
        )
    last = (rows[-1].device_id, rows[-1].fact_key) if rows and has_more else None
    return items, has_more, last
