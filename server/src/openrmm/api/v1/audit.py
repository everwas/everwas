"""Reading the audit log.

The log has been written to since M0 and had no reader at all: not an API
route, not a page. For a tool whose ordinary operations are "run this as root
on 400 machines" and "revoke that agent", a trail nobody can read is a trail
that does not exist.

Append-only, so there is nothing here but reads. Filters are the ones an
incident actually starts from: what happened to THIS machine, what did THIS
person do, what happened in the last hour.
"""

import uuid
from datetime import UTC, datetime, timedelta

from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession
from openrmm.models.audit import ActorType, AuditLog
from openrmm.schemas.audit import AuditEntryOut, AuditPage
from openrmm.security.tenancy import caller_org, scope_to_org

router = APIRouter()

#: Hard ceiling on a page. The table is the one thing here that grows without
#: bound, so an unbounded query is a way to take the API down by accident.
MAX_LIMIT = 200


@router.get("")
async def list_audit(
    db: DbSession,
    _user: CurrentUser,
    action: str | None = Query(default=None, description="exact action, e.g. device.retired"),
    actor: str | None = Query(default=None, description="exact actor id, usually an email"),
    actor_type: ActorType | None = None,
    target_id: str | None = Query(default=None, description="usually a device id"),
    hours: int | None = Query(default=None, ge=1, le=24 * 90),
    before: datetime | None = Query(default=None, description="cursor: older than this"),
    limit: int = Query(default=50, ge=1, le=MAX_LIMIT),
) -> AuditPage:
    # Scoped before any filter is applied. An entry names an operator and the
    # host they ran something on, so an unscoped list is another tenant's staff
    # directory and fleet inventory, reachable with ?target_id=<guessed uuid>.
    query = scope_to_org(
        select(AuditLog).order_by(AuditLog.at.desc(), AuditLog.id.desc()),
        AuditLog.org_id,
        caller_org(_user),
    )

    if action:
        query = query.where(AuditLog.action == action)
    if actor:
        query = query.where(AuditLog.actor_id == actor)
    if actor_type is not None:
        query = query.where(AuditLog.actor_type == actor_type)
    if target_id:
        query = query.where(AuditLog.target_id == target_id)
    if hours:
        query = query.where(AuditLog.at >= datetime.now(UTC) - timedelta(hours=hours))
    if before is not None:
        if before.tzinfo is None:
            raise HTTPException(
                status.HTTP_422_UNPROCESSABLE_ENTITY,
                "before must carry a timezone; a naive cursor silently pages by the "
                "server's local clock",
            )
        query = query.where(AuditLog.at < before)

    # One extra row tells us whether there is another page without a count(*)
    # over a table that only ever grows.
    rows = list((await db.execute(query.limit(limit + 1))).scalars())
    has_more = len(rows) > limit
    rows = rows[:limit]

    return AuditPage(
        entries=[AuditEntryOut.model_validate(r) for r in rows],
        has_more=has_more,
        next_before=rows[-1].at if rows and has_more else None,
    )


@router.get("/actions")
async def list_actions(db: DbSession, _user: CurrentUser) -> list[str]:
    """The action names actually present, for a filter that cannot be wrong.

    Typing an action by hand means typos return an empty page that looks
    exactly like "nothing happened".

    Scoped too: the set of actions another organization performs says what
    they do with the product, and it is the cheapest thing here to enumerate.
    """
    rows = await db.execute(
        scope_to_org(
            select(AuditLog.action).distinct().order_by(AuditLog.action),
            AuditLog.org_id,
            caller_org(_user),
        )
    )
    return list(rows.scalars())


@router.get("/device/{device_id}")
async def device_history(
    device_id: uuid.UUID,
    db: DbSession,
    _user: CurrentUser,
    limit: int = Query(default=50, ge=1, le=MAX_LIMIT),
) -> list[AuditEntryOut]:
    """Everything done to one machine, whoever did it.

    Scoped to the caller's organization: the audit trail names who ran what on
    which host, so leaking it across the boundary leaks both the existence of
    another tenant's fleet and the identities of their operators.

    Scoped on the AUDIT ROWS, not on the device. The log outlives its subjects
    deliberately — delete_device writes its entry before the row goes — so
    authorizing by loading the device made the history of a deleted machine
    unreadable at exactly the moment somebody needed it. The rows carry their
    own org_id (migration 0018) so the boundary does not depend on a parent
    that may be gone.

    Separate from the filtered list because it is the question an incident
    starts with, and because a device is named both as a target and, when the
    agent itself reports, as the actor.
    """
    rows = await db.execute(
        scope_to_org(
            select(AuditLog).where(
                (AuditLog.target_id == str(device_id))
                | ((AuditLog.actor_type == ActorType.agent) & (AuditLog.actor_id == str(device_id)))
            ),
            AuditLog.org_id,
            caller_org(_user),
        )
        .order_by(AuditLog.at.desc())
        .limit(limit)
    )
    entries = [AuditEntryOut.model_validate(r) for r in rows.scalars()]
    if not entries:
        # 404 rather than an empty list, so a device in another organization
        # reads exactly like one that never existed. An empty 200 would confirm
        # the UUID is not ours, which is the same tell the device routes avoid
        # by answering 404 instead of 403.
        raise HTTPException(status.HTTP_404_NOT_FOUND, "no audit history for this device")
    return entries
