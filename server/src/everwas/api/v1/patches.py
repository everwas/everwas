import uuid

from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import select

from everwas.api.deps import CurrentUser, DbSession, require_role
from everwas.bitemporal.query import get_facts
from everwas.models.device import Device
from everwas.models.patch import (
    ApprovalDecision,
    PatchApproval,
    PatchCatalog,
    PatchJob,
    PatchPolicy,
)
from everwas.models.user import Role
from everwas.schemas.patch import (
    ApprovalRequest,
    DeployRequest,
    DevicePatchOut,
    PatchJobOut,
    PatchOut,
    PatchPolicyIn,
    PatchPolicyOut,
)
from everwas.security.tenancy import caller_org, scope_to_org
from everwas.services.patching import (
    approve,
    approved_external_ids,
    queue_patch_install,
    queue_patch_scan,
)

router = APIRouter()
OPERATOR = require_role(Role.admin, Role.operator)


async def _device_or_404(db, device_id: uuid.UUID, user) -> Device:
    """Load a device the caller may act on. See devices.py for the reasoning:
    scoped in the query so a foreign device is never loaded, and 404 rather
    than 403 so its existence is not confirmed."""
    query = scope_to_org(
        select(Device).where(Device.id == device_id), Device.org_id, caller_org(user)
    )
    device = (await db.execute(query)).scalar_one_or_none()
    if device is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown device")
    return device


@router.get("")
async def list_catalog(
    db: DbSession, _user: CurrentUser, limit: int = Query(default=200, ge=1, le=1000)
) -> list[PatchOut]:
    rows = await db.execute(
        select(PatchCatalog).order_by(PatchCatalog.severity, PatchCatalog.title).limit(limit)
    )
    return [PatchOut.model_validate(p) for p in rows.scalars()]


@router.get("/device/{device_id}")
async def device_patches(
    device_id: uuid.UUID, db: DbSession, _user: CurrentUser
) -> list[DevicePatchOut]:
    """What this device currently reports as pending, joined to the catalog."""
    device = await _device_or_404(db, device_id, _user)
    facts = await get_facts(db, "patchstate", device_id)
    if not facts:
        return []

    external_ids = [f["fact_key"].removeprefix("patch:") for f in facts]
    catalog = {
        c.external_id: c
        for c in (
            await db.execute(
                select(PatchCatalog).where(
                    PatchCatalog.os_family == device.os_family,
                    PatchCatalog.external_id.in_(external_ids),
                )
            )
        ).scalars()
    }
    approved = set(await approved_external_ids(db, device, external_ids))

    out: list[DevicePatchOut] = []
    for fact in facts:
        ext = fact["fact_key"].removeprefix("patch:")
        payload = fact["payload"] or {}
        entry = catalog.get(ext)
        out.append(
            DevicePatchOut(
                id=entry.id if entry else uuid.uuid5(uuid.NAMESPACE_URL, ext),
                os_family=device.os_family,
                external_id=ext,
                title=(entry.title if entry else payload.get("title", "")) or ext,
                kind=(entry.kind if entry else payload.get("kind", "other")),
                severity=(entry.severity if entry else payload.get("severity", "unknown")),
                kb_ids=entry.kb_ids if entry else [],
                cves=entry.cves if entry else [],
                size_bytes=entry.size_bytes if entry else payload.get("size_bytes"),
                reboot_likely=bool(entry.reboot_likely if entry else payload.get("reboot_likely")),
                approved=ext in approved,
                unsupported=bool(payload.get("unsupported")),
                detail=str(payload.get("detail", "")),
            )
        )
    return out


@router.post("/approve", dependencies=[OPERATOR])
async def approve_patches(body: ApprovalRequest, db: DbSession, user: CurrentUser) -> dict:
    try:
        decision = ApprovalDecision(body.decision)
    except ValueError as exc:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "bad decision") from exc

    for patch_id in body.patch_ids:
        await approve(
            db,
            patch_id,
            device_id=body.device_id,
            decision=decision,
            decided_by=user.email,
            org_id=user.org_id,
        )
    await db.commit()
    return {"decided": len(body.patch_ids), "decision": decision.value}


@router.post("/scan/{device_id}", dependencies=[OPERATOR])
async def scan_device(device_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> dict:
    device = await _device_or_404(db, device_id, _user)
    job_id = await queue_patch_scan(db, device)
    await db.commit()
    return {"job_id": str(job_id), "device_id": str(device_id)}


@router.post("/deploy", dependencies=[OPERATOR])
async def deploy(body: DeployRequest, db: DbSession, user: CurrentUser) -> PatchJobOut:
    device = await _device_or_404(db, body.device_id, user)
    allowed = await approved_external_ids(db, device, body.external_ids or None)
    if body.external_ids:
        # Never install something that was not approved, even if asked to.
        requested = set(body.external_ids)
        allowed = [e for e in allowed if e in requested]
        if refused := requested - set(allowed):
            raise HTTPException(
                status.HTTP_400_BAD_REQUEST,
                f"{len(refused)} patch(es) are not approved for this device",
            )
    if not allowed:
        raise HTTPException(status.HTTP_400_BAD_REQUEST, "no approved patches for this device")

    job = await queue_patch_install(db, device, allowed, requested_by=user.email)
    await db.commit()
    return PatchJobOut.model_validate(job)


@router.get("/jobs")
async def list_jobs(
    db: DbSession,
    _user: CurrentUser,
    device_id: uuid.UUID | None = None,
    limit: int = Query(default=50, ge=1, le=200),
) -> list[PatchJobOut]:
    stmt = select(PatchJob).order_by(PatchJob.queued_at.desc()).limit(limit)
    if device_id is not None:
        stmt = stmt.where(PatchJob.device_id == device_id)
    return [PatchJobOut.model_validate(j) for j in (await db.execute(stmt)).scalars()]


# --- policies ---


@router.get("/policies")
async def list_policies(db: DbSession, _user: CurrentUser) -> list[PatchPolicyOut]:
    rows = await db.execute(select(PatchPolicy).order_by(PatchPolicy.name))
    return [PatchPolicyOut.model_validate(p) for p in rows.scalars()]


@router.post("/policies", status_code=status.HTTP_201_CREATED, dependencies=[OPERATOR])
async def create_policy(body: PatchPolicyIn, db: DbSession, _user: CurrentUser) -> PatchPolicyOut:
    policy = PatchPolicy(**body.model_dump())
    db.add(policy)
    await db.commit()
    return PatchPolicyOut.model_validate(policy)


@router.delete(
    "/policies/{policy_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[OPERATOR]
)
async def delete_policy(policy_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> None:
    policy = (
        await db.execute(select(PatchPolicy).where(PatchPolicy.id == policy_id))
    ).scalar_one_or_none()
    if policy is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown policy")
    await db.delete(policy)
    await db.commit()


@router.get("/approvals")
async def list_approvals(
    db: DbSession, _user: CurrentUser, device_id: uuid.UUID | None = None
) -> list[dict]:
    stmt = select(PatchApproval, PatchCatalog).join(
        PatchCatalog, PatchCatalog.id == PatchApproval.patch_id
    )
    if device_id is not None:
        stmt = stmt.where(
            (PatchApproval.device_id == device_id) | (PatchApproval.device_id.is_(None))
        )
    rows = await db.execute(stmt)
    return [
        {
            "patch_id": str(a.patch_id),
            "external_id": c.external_id,
            "title": c.title,
            "decision": a.decision.value,
            "decided_by": a.decided_by,
            "device_id": str(a.device_id) if a.device_id else None,
        }
        for a, c in rows
    ]
