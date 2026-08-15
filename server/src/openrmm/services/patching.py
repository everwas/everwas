"""Patch catalog, approvals, and deployment.

Approval semantics differ per OS family and we do not pretend otherwise: on
Windows a patch is an individually approvable update; on apt/dnf "approving"
means allowing a specific package upgrade. Both normalize to
patch_catalog.external_id, which is unique only within an OS family.
"""

import uuid
from datetime import UTC, datetime

import nats
import structlog
from sqlalchemy import select
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.device import Device, OsFamily
from openrmm.models.patch import (
    ApprovalDecision,
    PatchApproval,
    PatchCatalog,
    PatchJob,
    PatchJobStatus,
    PatchPolicy,
)
from openrmm.natsio.subjects import jobs_queue
from openrmm.services.jobs import job_envelope
from openrmm.util.ids import uuid7

log = structlog.get_logger()


async def upsert_catalog(
    db: AsyncSession, os_family: OsFamily, patches: list[dict]
) -> dict[str, uuid.UUID]:
    """Record every patch an agent reported. Returns external_id -> catalog id."""
    if not patches:
        return {}

    rows = [
        {
            "id": uuid7(),
            "os_family": os_family,
            "external_id": str(p["id"])[:255],
            "title": str(p.get("title", ""))[:2000],
            "kind": str(p.get("kind", "other"))[:32],
            "severity": str(p.get("severity", "unknown")),
            "kb_ids": [str(k) for k in (p.get("kb_ids") or [])],
            "cves": [str(c) for c in (p.get("cves") or [])],
            "size_bytes": p.get("size_bytes"),
            "reboot_likely": bool(p.get("reboot_likely")),
        }
        for p in patches
        if p.get("id")
    ]
    if not rows:
        return {}

    stmt = pg_insert(PatchCatalog.__table__).values(rows)
    # Titles and severities get refined as backends learn more; keep the row.
    stmt = stmt.on_conflict_do_update(
        constraint="uq_patch_catalog_native",
        set_={
            "title": stmt.excluded.title,
            "severity": stmt.excluded.severity,
            "kind": stmt.excluded.kind,
            "reboot_likely": stmt.excluded.reboot_likely,
        },
    )
    await db.execute(stmt)

    external_ids = [r["external_id"] for r in rows]
    found = await db.execute(
        select(PatchCatalog.external_id, PatchCatalog.id).where(
            PatchCatalog.os_family == os_family, PatchCatalog.external_id.in_(external_ids)
        )
    )
    return {ext: pid for ext, pid in found}


async def approve(
    db: AsyncSession,
    patch_id: uuid.UUID,
    *,
    device_id: uuid.UUID | None,
    decision: ApprovalDecision,
    decided_by: str,
    policy_id: uuid.UUID | None = None,
) -> PatchApproval:
    stmt = (
        pg_insert(PatchApproval.__table__)
        .values(
            id=uuid7(),
            patch_id=patch_id,
            device_id=device_id,
            policy_id=policy_id,
            decision=decision.value,
            decided_by=decided_by,
            decided_at=datetime.now(UTC),
        )
        .on_conflict_do_update(
            constraint="uq_patch_approval_scope",
            set_={
                "decision": decision.value,
                "decided_by": decided_by,
                "decided_at": datetime.now(UTC),
            },
        )
        .returning(PatchApproval.__table__.c.id)
    )
    approval_id = (await db.execute(stmt)).scalar_one()
    db.add(
        AuditLog(
            actor_type=ActorType.user,
            actor_id=decided_by,
            action=f"patch.{decision.value}",
            target_type="patch",
            target_id=str(patch_id),
            detail={"device_id": str(device_id) if device_id else None},
        )
    )
    return (
        await db.execute(select(PatchApproval).where(PatchApproval.id == approval_id))
    ).scalar_one()


async def approved_external_ids(
    db: AsyncSession, device: Device, candidate_ids: list[str] | None = None
) -> list[str]:
    """External ids approved for this device (device-scoped or fleet-wide)."""
    stmt = (
        select(PatchCatalog.external_id)
        .join(PatchApproval, PatchApproval.patch_id == PatchCatalog.id)
        .where(
            PatchCatalog.os_family == device.os_family,
            PatchApproval.decision == ApprovalDecision.approved,
            (PatchApproval.device_id == device.id) | (PatchApproval.device_id.is_(None)),
        )
    )
    if candidate_ids:
        stmt = stmt.where(PatchCatalog.external_id.in_(candidate_ids))
    return list((await db.execute(stmt)).scalars())


async def queue_patch_scan(db: AsyncSession, nc: nats.NATS, device: Device) -> uuid.UUID:
    js = nc.jetstream()
    job_id = uuid7()
    await js.publish(
        jobs_queue(str(device.id)),
        job_envelope(str(device.id), str(job_id), "patch.scan", {}),
        headers={"Nats-Msg-Id": str(job_id)},
    )
    return job_id


async def queue_patch_install(
    db: AsyncSession,
    nc: nats.NATS,
    device: Device,
    external_ids: list[str],
    *,
    requested_by: str,
    policy_id: uuid.UUID | None = None,
) -> PatchJob:
    """Deploy approved patches. Callers must pass only approved ids."""
    job = PatchJob(
        id=uuid7(),
        device_id=device.id,
        policy_id=policy_id,
        external_ids=external_ids,
        status=PatchJobStatus.queued,
        requested_by=requested_by,
    )
    db.add(job)
    await db.flush()

    js = nc.jetstream()
    await js.publish(
        jobs_queue(str(device.id)),
        job_envelope(
            str(device.id),
            str(job.id),
            "patch.install",
            {"ids": external_ids, "requested_by": requested_by},
        ),
        headers={"Nats-Msg-Id": str(job.id)},
    )
    db.add(
        AuditLog(
            actor_type=ActorType.user,
            actor_id=requested_by,
            action="patch.install_queued",
            target_type="device",
            target_id=str(device.id),
            detail={"count": len(external_ids), "job_id": str(job.id)},
        )
    )
    log.info(
        "patch install queued",
        device=device.hostname,
        count=len(external_ids),
        job_id=str(job.id),
    )
    return job


async def auto_approve_for_policies(db: AsyncSession, device: Device, patches: list[dict]) -> int:
    """Apply each matching policy's auto_approve_severities to a fresh scan."""
    from openrmm.services.jobs import resolve_targets

    policies = list(
        (await db.execute(select(PatchPolicy).where(PatchPolicy.enabled.is_(True)))).scalars()
    )
    if not policies:
        return 0

    approved = 0
    for policy in policies:
        if not policy.auto_approve_severities:
            continue
        targets = await resolve_targets(db, policy.target or {})
        if device.id not in {d.id for d in targets}:
            continue
        wanted = {s.lower() for s in policy.auto_approve_severities}
        matching = [p for p in patches if str(p.get("severity", "unknown")).lower() in wanted]
        if not matching:
            continue
        catalog = await upsert_catalog(db, device.os_family, matching)
        for patch_id in catalog.values():
            await approve(
                db,
                patch_id,
                device_id=device.id,
                decision=ApprovalDecision.approved,
                decided_by=f"policy:{policy.name}",
                policy_id=policy.id,
            )
            approved += 1
    return approved
