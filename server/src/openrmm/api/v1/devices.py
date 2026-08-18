import json
import uuid
from datetime import UTC, datetime, timedelta
from typing import Literal

import nats.errors
import structlog
from fastapi import APIRouter, HTTPException, Query, status
from pydantic import BaseModel
from sqlalchemy import func, select

from openrmm.api.deps import CurrentUser, DbSession, require_role
from openrmm.bitemporal.query import get_facts
from openrmm.config import get_settings
from openrmm.models.device import Device, DeviceStatus
from openrmm.models.facts import FactKind, FactNetwork
from openrmm.models.script import RunStatus, RunTrigger, ScriptRun
from openrmm.models.telemetry import DeviceSnapshot, DeviceStatusLatest, telemetry_metrics
from openrmm.models.user import Role
from openrmm.natsio.agent_request import request_agent
from openrmm.natsio.client import get_nats
from openrmm.schemas.device import (
    AgentUpdateRequest,
    DeviceDetailOut,
    DeviceOut,
    FactOut,
    NetInterfaceSeries,
    NetRatePoint,
    TelemetryPoint,
)
from openrmm.security.tenancy import caller_org, scope_to_org
from openrmm.services.certificates import certificate_drift
from openrmm.services.devices import DeviceNotRetiredError, delete_device
from openrmm.services.enrollment import (
    ROTATION_GRACE,
    RotationInFlightError,
    retire_device,
    rotate_agent_secret,
)
from openrmm.services.network_telemetry import interface_rates

router = APIRouter()
log = structlog.get_logger()

ADMIN = require_role(Role.admin)
OPERATOR = require_role(Role.admin, Role.operator)


async def _device_or_404(db, device_id: uuid.UUID, user) -> Device:
    """Load a device the caller is allowed to act on.

    Takes the user, and is the only way these routes reach a Device, so the
    organization filter cannot be omitted at one route and present at the rest.
    A device in another organization returns 404, not 403: a caller who is not
    entitled to it should not learn whether it exists.
    """
    query = scope_to_org(
        select(Device).where(Device.id == device_id), Device.org_id, caller_org(user)
    )
    device = (await db.execute(query)).scalar_one_or_none()
    if device is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown device")
    return device


@router.get("")
async def list_devices(
    db: DbSession, _user: CurrentUser, include_retired: bool = False
) -> list[DeviceOut]:
    """The fleet. Retired devices are excluded unless asked for.

    A retired device is one an operator has deliberately taken out of service.
    Leaving them in the default list means every device picker, every count and
    every table is padded with machines that will never report again, and the
    ones that matter get harder to find as the list grows.
    """
    query = scope_to_org(select(Device).order_by(Device.hostname), Device.org_id, caller_org(_user))
    if not include_retired:
        query = query.where(Device.status != DeviceStatus.retired)
    rows = await db.execute(query)
    return [DeviceOut.model_validate(d) for d in rows.scalars()]


class CertificateDriftOut(BaseModel):
    device_id: uuid.UUID
    hostname: str
    #: unknown | stale | missing
    kind: str
    reported_serial: str | None
    issued_serial: str | None
    reported_at: datetime | None


@router.get("/certificate-drift")
async def list_certificate_drift(db: DbSession, _user: CurrentUser) -> list[CertificateDriftOut]:
    """Devices holding a different certificate from the one we last issued.

    Knowing what we issued and knowing what the machine has are different
    facts, and the gap between them is invisible until it surfaces as an
    authentication failure nobody can account for. Three things show up here:
    a renewal that was issued and never saved, a machine restored from a backup
    image or cloned from a template, and material deleted by hand.

    Retired devices are excluded. They are meant to stop reporting, so drift on
    a machine deliberately taken out of service is not a finding.
    """
    query = scope_to_org(
        select(Device).where(Device.status != DeviceStatus.retired),
        Device.org_id,
        caller_org(_user),
    )
    return [
        CertificateDriftOut(
            device_id=d.device_id,
            hostname=d.hostname,
            kind=str(d.kind),
            reported_serial=d.reported_serial,
            issued_serial=d.issued_serial,
            reported_at=d.reported_at,
        )
        for d in await certificate_drift(db, query)
    ]


# Registered BEFORE /{device_id}. FastAPI matches routes in registration order,
# so a literal path declared after a path parameter that accepts it is
# unreachable: "certificate-drift" would be parsed as a device id and rejected
# as a malformed UUID, and the endpoint would 422 forever while looking
# perfectly correct in the source.
@router.get("/{device_id}")
async def get_device(device_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> DeviceDetailOut:
    device = await _device_or_404(db, device_id, _user)
    latest = (
        await db.execute(
            select(DeviceStatusLatest).where(DeviceStatusLatest.device_id == device_id)
        )
    ).scalar_one_or_none()
    out = DeviceDetailOut.model_validate(device)
    if latest is not None:
        out.cpu_pct = latest.cpu_pct
        out.mem_pct = latest.mem_pct
        out.worst_disk_pct = latest.worst_disk_pct
    return out


@router.get("/{device_id}/telemetry")
async def get_telemetry(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    hours: int = Query(default=24, ge=1, le=168),
) -> list[TelemetryPoint]:
    await _device_or_404(db, device_id, _user)
    since = datetime.now(UTC) - timedelta(hours=hours)
    t = telemetry_metrics
    rows = await db.execute(
        select(t.c.ts, t.c.cpu_pct, t.c.mem_used, t.c.mem_total, t.c.load1)
        .where(t.c.device_id == device_id, t.c.ts >= since)
        .order_by(t.c.ts)
    )
    return [
        TelemetryPoint(
            ts=r.ts,
            cpu_pct=r.cpu_pct,
            mem_pct=(r.mem_used / r.mem_total * 100.0) if r.mem_used and r.mem_total else None,
            load1=r.load1,
        )
        for r in rows
    ]


@router.get("/{device_id}/network")
async def get_network(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    hours: int = Query(default=24, ge=1, le=168),
) -> list[NetInterfaceSeries]:
    """Per-interface throughput, labelled from the current inventory."""
    await _device_or_404(db, device_id, _user)
    since = datetime.now(UTC) - timedelta(hours=hours)
    series = await interface_rates(db, device_id, since)

    # Current beliefs about the interfaces themselves: MAC, up/down, addresses.
    facts = (
        await db.execute(
            select(FactNetwork.fact_key, FactNetwork.payload).where(
                FactNetwork.device_id == device_id,
                func.upper_inf(FactNetwork.recorded_during),
                # Both filters are required. upper_inf(recorded_during) alone
                # means "a belief we still hold", which includes CORRECTION
                # rows: after any change there are two such rows for a fact
                # key, one describing the value that ended. Without the
                # valid_during filter the dict comprehension below keeps
                # whichever row Postgres returned last, so an interface that
                # changed address shows the old one or the new one depending
                # on physical row order, and flips after a VACUUM.
                func.upper_inf(FactNetwork.valid_during),
            )
        )
    ).all()
    meta = {k.removeprefix("iface:"): p for k, p in facts}

    # Union, not intersection. An interface that exists but has no traffic
    # history still belongs on the page (it is how you see a NIC is down), and
    # one with history that has since disappeared is exactly what someone
    # investigating a network change is looking for.
    names = sorted(set(series) | set(meta))
    return [
        NetInterfaceSeries(
            name=name,
            mac=(meta.get(name) or {}).get("mac"),
            up=(meta.get(name) or {}).get("up"),
            addresses=(meta.get(name) or {}).get("addresses") or [],
            points=[NetRatePoint(**p) for p in series.get(name, [])],
        )
        for name in names
    ]


@router.get("/{device_id}/facts")
async def get_device_facts(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    kind: FactKind = "hardware",
    as_of: datetime | None = None,
    knew_at: datetime | None = None,
) -> list[FactOut]:
    await _device_or_404(db, device_id, _user)
    facts = await get_facts(db, kind, device_id, as_of=as_of, knew_at=knew_at)
    return [FactOut(**f) for f in facts]


@router.get("/{device_id}/snapshots/{kind}")
async def get_snapshot(
    device_id: uuid.UUID,
    kind: Literal["processes", "services"],
    db: DbSession,
    _user: CurrentUser,
) -> dict:
    await _device_or_404(db, device_id, _user)
    snap = (
        await db.execute(
            select(DeviceSnapshot).where(
                DeviceSnapshot.device_id == device_id, DeviceSnapshot.kind == kind
            )
        )
    ).scalar_one_or_none()
    if snap is None:
        return {"payload": None, "updated_at": None}
    return {"payload": snap.payload, "updated_at": snap.updated_at.isoformat()}


@router.post("/{device_id}/retire", dependencies=[ADMIN])
async def retire(device_id: uuid.UUID, db: DbSession, user: CurrentUser) -> dict:
    """Revoke an agent permanently.

    Admin-only and irreversible: the credential is deleted, so the machine
    cannot reconnect and cannot re-enroll without a fresh enrollment token.

    The response reports `revoked_within_s` rather than claiming the agent is
    gone. Authorization is decided at CONNECT, so a currently-connected agent
    keeps working until its JWT expires. Saying so is the difference between
    an operator who waits and one who assumes and moves on.
    """
    # Through the scoped gate first, so this route cannot be the one that
    # forgets. retire_device looks up by primary key alone, which is correct
    # for a service that trusts its caller; the caller is here.
    await _device_or_404(db, device_id, user)
    device = await retire_device(db, device_id, actor=user.email)
    if device is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown device")
    await db.commit()

    ttl = get_settings().nats_jwt_ttl_s
    log.info("device retired", device_id=str(device_id), actor=user.email)
    return {
        "device_id": str(device_id),
        "status": device.status.value,
        "revoked_within_s": ttl,
        "detail": (
            "credentials deleted; an already-connected agent keeps its session "
            f"until its JWT expires, at most {ttl}s from now"
        ),
    }


@router.delete("/{device_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[ADMIN])
async def delete(device_id: uuid.UUID, db: DbSession, user: CurrentUser) -> None:
    """Remove a retired device and all of its history. Not reversible.

    Retiring is the first step and this is the second. Requiring both means a
    live machine cannot be erased by one wrong click, and the retire in
    between gives the agent's credential time to be revoked before its records
    disappear.
    """
    await _device_or_404(db, device_id, user)
    try:
        device = await delete_device(db, device_id, actor=user.email)
    except DeviceNotRetiredError as exc:
        raise HTTPException(status.HTTP_409_CONFLICT, str(exc)) from exc
    if device is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown device")
    await db.commit()


@router.post("/{device_id}/rotate-credentials", dependencies=[ADMIN])
async def rotate_credentials(
    device_id: uuid.UUID, db: DbSession, user: CurrentUser, force: bool = False
) -> dict:
    """Issue a new agent secret and hand it to the agent.

    Both secrets are valid during the grace window, so this is safe to have
    interrupted. It is NOT safe to repeat: only one generation of history is
    kept, so a second rotation over an unconfirmed one throws away the secret
    the agent is still using. That is refused with a 409 unless `force`.

    The commit happens whether or not the agent answered: rolling back on a
    failed delivery would leave the audit trail saying nothing was attempted
    while the agent may already hold the new secret.
    """
    device = await _device_or_404(db, device_id, user)
    try:
        secret = await rotate_agent_secret(db, device_id, actor=user.email, force=force)
    except RotationInFlightError as exc:
        # 409, not 202: pressing rotate again after a `delivered: false` is the
        # natural reaction and it is the one thing that bricks the agent.
        raise HTTPException(status.HTTP_409_CONFLICT, str(exc)) from exc
    if secret is None:
        raise HTTPException(status.HTTP_409_CONFLICT, "device has no credentials to rotate")

    delivered, detail = False, "agent did not answer; it keeps its current secret"
    try:
        raw = await request_agent(
            get_nats(),
            str(device_id),
            "agent.rotate_creds",
            json.dumps({"agent_secret": secret, "requested_by": user.email}).encode(),
        )
        answer = json.loads(raw)
        delivered = bool(answer.get("accepted"))
        if not delivered:
            detail = str(answer.get("error") or "agent refused the rotation")
    except Exception as exc:
        detail = f"delivery failed: {exc}"
        log.warning("rotate delivery failed", device_id=str(device_id), err=str(exc))

    # Commit either way. The old secret still works for the grace window, so
    # an undelivered rotation costs nothing and a half-delivered one recovers.
    await db.commit()

    grace = int(ROTATION_GRACE.total_seconds())
    return {
        "device_id": str(device_id),
        "hostname": device.hostname,
        "delivered": delivered,
        "previous_secret_valid_for_s": grace,
        "detail": detail if not delivered else "agent acknowledged the new secret",
    }


@router.post("/{device_id}/update", dependencies=[OPERATOR])
async def update_agent(
    device_id: uuid.UUID, body: AgentUpdateRequest, db: DbSession, user: CurrentUser
) -> dict:
    """Ask an agent to update itself.

    A run row is created BEFORE the command is sent. The agent publishes its
    result to a job id, and result ingest drops results for job ids it does
    not know, so creating the row afterwards races the agent on a fast link
    and loses the outcome of the update.
    """
    device = await _device_or_404(db, device_id, user)
    if device.status is DeviceStatus.retired:
        raise HTTPException(status.HTTP_409_CONFLICT, "device is retired")

    run = ScriptRun(
        id=uuid.uuid4(),
        device_id=device_id,
        trigger=RunTrigger.manual,
        status=RunStatus.queued,
        requested_by=user.email,
    )
    db.add(run)
    await db.commit()

    # mode="json" because artifact_url and signature_url are HttpUrl, which
    # json.dumps cannot encode. Without it the failure lands in the same
    # except as an unreachable agent and gets reported as one, which sends an
    # operator to look at a machine that is fine.
    payload = body.model_dump(mode="json", exclude_none=True)
    payload |= {"job_id": str(run.id), "requested_by": user.email}
    try:
        raw = await request_agent(
            get_nats(), str(device_id), "agent.update", json.dumps(payload).encode()
        )
        answer = json.loads(raw)
    except nats.errors.TimeoutError as exc:
        run.status = RunStatus.failed
        run.stderr = "the agent did not answer; it may be offline"
        run.finished_at = datetime.now(UTC)
        await db.commit()
        raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "agent is not reachable") from exc
    except Exception as exc:
        # Anything else is ours, not the agent's. Say so: "agent unreachable"
        # for a server-side bug costs someone a trip to a machine that is fine.
        log.exception("agent update dispatch failed", device_id=str(device_id))
        run.status = RunStatus.failed
        run.stderr = f"the server could not dispatch the update: {exc}"
        run.finished_at = datetime.now(UTC)
        await db.commit()
        raise HTTPException(
            status.HTTP_500_INTERNAL_SERVER_ERROR, "could not dispatch the update"
        ) from exc

    if not answer.get("accepted"):
        # A refusal is terminal now, not a job that hangs at queued for ever.
        run.status = RunStatus.failed
        run.stderr = str(answer.get("error") or "agent refused the update")
        run.finished_at = datetime.now(UTC)
        await db.commit()
        return {"job_id": str(run.id), "accepted": False, "error": run.stderr}

    run.status = RunStatus.running
    run.started_at = datetime.now(UTC)
    await db.commit()
    log.info("agent update dispatched", device_id=str(device_id), version=body.version)
    return {"job_id": str(run.id), "accepted": True, "version": body.version}
