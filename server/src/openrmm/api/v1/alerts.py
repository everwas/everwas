import uuid
from datetime import UTC, datetime

from fastapi import APIRouter, HTTPException, Query, status
from sqlalchemy import delete, select

from openrmm.api.deps import CurrentUser, DbSession, require_role
from openrmm.models.alert import (
    Alert,
    AlertRule,
    AlertState,
    NotificationChannel,
    NotificationOutbox,
    OutboxStatus,
    RuleChannel,
)
from openrmm.models.audit import ActorType, AuditLog
from openrmm.models.user import Role
from openrmm.schemas.alert import (
    SECRET_CONFIG_KEYS,
    AlertOut,
    AlertRuleIn,
    AlertRuleOut,
    ChannelIn,
    ChannelOut,
    OutboxOut,
)
from openrmm.services.outbox import outbox_health

router = APIRouter()
OPERATOR = require_role(Role.admin, Role.operator)


async def _channel_ids(db, rule_id: uuid.UUID) -> list[uuid.UUID]:
    rows = await db.execute(select(RuleChannel.channel_id).where(RuleChannel.rule_id == rule_id))
    return list(rows.scalars())


def _rule_out(rule: AlertRule, channel_ids: list[uuid.UUID]) -> AlertRuleOut:
    out = AlertRuleOut.model_validate(rule)
    out.channel_ids = channel_ids
    return out


NO_CHANNELS = (
    "an enabled rule with no notification channels fires into the void: it opens "
    "alerts nobody is told about. Attach at least one channel, or save the rule "
    "with enabled=false until you have one."
)


async def _validate_channels(db, body: AlertRuleIn) -> None:
    """A rule that can never notify anyone is a configuration error, not a choice.

    This is the quiet-failure shape the whole alerting review is about: the
    rule fires forever, the alerts table fills up, and the operator is never
    told because there was nowhere to tell them.
    """
    if body.enabled and not body.channel_ids:
        raise HTTPException(status.HTTP_422_UNPROCESSABLE_CONTENT, NO_CHANNELS)
    if not body.channel_ids:
        return
    known = set(
        (
            await db.execute(
                select(NotificationChannel.id).where(NotificationChannel.id.in_(body.channel_ids))
            )
        ).scalars()
    )
    if missing := [str(cid) for cid in body.channel_ids if cid not in known]:
        raise HTTPException(
            status.HTTP_422_UNPROCESSABLE_CONTENT,
            f"unknown notification channel(s): {', '.join(sorted(missing))}",
        )


# --- rules ---


@router.get("/rules")
async def list_rules(db: DbSession, _user: CurrentUser) -> list[AlertRuleOut]:
    rules = list((await db.execute(select(AlertRule).order_by(AlertRule.name))).scalars())
    return [_rule_out(r, await _channel_ids(db, r.id)) for r in rules]


@router.post("/rules", status_code=status.HTTP_201_CREATED, dependencies=[OPERATOR])
async def create_rule(body: AlertRuleIn, db: DbSession, user: CurrentUser) -> AlertRuleOut:
    await _validate_channels(db, body)
    data = body.model_dump(exclude={"channel_ids"})
    rule = AlertRule(**data)
    db.add(rule)
    await db.flush()
    for channel_id in body.channel_ids:
        db.add(RuleChannel(rule_id=rule.id, channel_id=channel_id))
    db.add(
        AuditLog(
            org_id=user.org_id,
            actor_type=ActorType.user,
            actor_id=user.email,
            action="alert_rule.created",
            target_type="alert_rule",
            target_id=str(rule.id),
            detail={"name": rule.name, "metric": rule.metric.value},
        )
    )
    await db.commit()
    return _rule_out(rule, body.channel_ids)


@router.put("/rules/{rule_id}", dependencies=[OPERATOR])
async def update_rule(
    rule_id: uuid.UUID, body: AlertRuleIn, db: DbSession, _user: CurrentUser
) -> AlertRuleOut:
    rule = (await db.execute(select(AlertRule).where(AlertRule.id == rule_id))).scalar_one_or_none()
    if rule is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown rule")
    # Same check as create: an edit is just as capable of silencing a rule.
    await _validate_channels(db, body)
    for field, value in body.model_dump(exclude={"channel_ids"}).items():
        setattr(rule, field, value)
    await db.execute(delete(RuleChannel).where(RuleChannel.rule_id == rule_id))
    for channel_id in body.channel_ids:
        db.add(RuleChannel(rule_id=rule_id, channel_id=channel_id))
    await db.commit()
    return _rule_out(rule, body.channel_ids)


@router.delete("/rules/{rule_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[OPERATOR])
async def delete_rule(rule_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> None:
    rule = (await db.execute(select(AlertRule).where(AlertRule.id == rule_id))).scalar_one_or_none()
    if rule is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown rule")
    await db.delete(rule)
    await db.commit()


# --- alerts ---


@router.get("")
async def list_alerts(
    db: DbSession,
    _user: CurrentUser,
    state: AlertState | None = None,
    device_id: uuid.UUID | None = None,
    limit: int = Query(default=100, ge=1, le=500),
) -> list[AlertOut]:
    stmt = select(Alert).order_by(Alert.opened_at.desc()).limit(limit)
    if state is not None:
        stmt = stmt.where(Alert.state == state)
    if device_id is not None:
        stmt = stmt.where(Alert.device_id == device_id)
    return [AlertOut.model_validate(a) for a in (await db.execute(stmt)).scalars()]


@router.post("/{alert_id}/ack", dependencies=[OPERATOR])
async def ack_alert(alert_id: uuid.UUID, db: DbSession, user: CurrentUser) -> AlertOut:
    alert = (await db.execute(select(Alert).where(Alert.id == alert_id))).scalar_one_or_none()
    if alert is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown alert")
    if alert.state == AlertState.resolved:
        raise HTTPException(status.HTTP_409_CONFLICT, "alert already resolved")
    alert.state = AlertState.acknowledged
    alert.acked_at = datetime.now(UTC)
    alert.acked_by = user.email
    db.add(
        AuditLog(
            org_id=user.org_id,
            actor_type=ActorType.user,
            actor_id=user.email,
            action="alert.acknowledged",
            target_type="alert",
            target_id=str(alert.id),
        )
    )
    await db.commit()
    return AlertOut.model_validate(alert)


@router.post("/{alert_id}/resolve", dependencies=[OPERATOR])
async def resolve_alert(alert_id: uuid.UUID, db: DbSession, user: CurrentUser) -> AlertOut:
    alert = (await db.execute(select(Alert).where(Alert.id == alert_id))).scalar_one_or_none()
    if alert is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown alert")
    alert.state = AlertState.resolved
    alert.resolved_at = datetime.now(UTC)
    db.add(
        AuditLog(
            org_id=user.org_id,
            actor_type=ActorType.user,
            actor_id=user.email,
            action="alert.resolved",
            target_type="alert",
            target_id=str(alert.id),
        )
    )
    await db.commit()
    return AlertOut.model_validate(alert)


# --- channels ---


@router.get("/channels")
async def list_channels(db: DbSession, _user: CurrentUser) -> list[ChannelOut]:
    rows = await db.execute(select(NotificationChannel).order_by(NotificationChannel.name))
    # redacted(), never model_validate(). ChannelOut.config is a plain dict, so
    # validating straight off the model serialises the webhook signing secret
    # and the gotify token to whoever asked, and this route required only *a*
    # user, which by default is a viewer. Nothing in the product needs the
    # plaintext: the edit form cannot echo a credential back, which is why
    # update_channel treats an absent key as unchanged.
    return [ChannelOut.redacted(c) for c in rows.scalars()]


@router.post("/channels", status_code=status.HTTP_201_CREATED, dependencies=[OPERATOR])
async def create_channel(body: ChannelIn, db: DbSession, _user: CurrentUser) -> ChannelOut:
    channel = NotificationChannel(**body.model_dump())
    db.add(channel)
    await db.commit()
    # The caller just sent this credential, so echoing it is not a disclosure
    # to them; it is redacted anyway so that no response body on this router
    # ever carries one. A single exception is how the next reader concludes
    # that sometimes it is fine.
    return ChannelOut.redacted(channel)


@router.put("/channels/{channel_id}", dependencies=[OPERATOR])
async def update_channel(
    channel_id: uuid.UUID, body: ChannelIn, db: DbSession, _user: CurrentUser
) -> ChannelOut:
    """Edit a channel, PRESERVING any credential the caller did not supply.

    The browser is never sent the webhook secret or the gotify token, so an
    edit form cannot echo them back. Treating an absent key as "clear it"
    would mean renaming a channel silently breaks its authentication, and the
    only symptom is deliveries failing a signature check somewhere else.
    Absent means unchanged; to change a credential, send it.
    """
    channel = (
        await db.execute(select(NotificationChannel).where(NotificationChannel.id == channel_id))
    ).scalar_one_or_none()
    if channel is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown channel")

    merged = dict(body.config or {})
    for key in SECRET_CONFIG_KEYS:
        if not merged.get(key) and (channel.config or {}).get(key):
            merged[key] = channel.config[key]

    channel.name = body.name
    channel.kind = body.kind
    channel.config = merged
    channel.enabled = body.enabled
    await db.commit()
    return ChannelOut.redacted(channel)


@router.delete(
    "/channels/{channel_id}", status_code=status.HTTP_204_NO_CONTENT, dependencies=[OPERATOR]
)
async def delete_channel(channel_id: uuid.UUID, db: DbSession, _user: CurrentUser) -> None:
    channel = (
        await db.execute(select(NotificationChannel).where(NotificationChannel.id == channel_id))
    ).scalar_one_or_none()
    if channel is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown channel")
    await db.delete(channel)
    await db.commit()


@router.post("/channels/{channel_id}/test", dependencies=[OPERATOR])
async def test_channel(channel_id: uuid.UUID, db: DbSession, user: CurrentUser) -> OutboxOut:
    """Enqueue a test notification; the dispatcher's drainer delivers it."""
    channel = (
        await db.execute(select(NotificationChannel).where(NotificationChannel.id == channel_id))
    ).scalar_one_or_none()
    if channel is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown channel")
    entry = NotificationOutbox(
        channel_id=channel_id,
        status=OutboxStatus.pending,
        payload={
            "kind": "test",
            "title": "OpenRMM test notification",
            "body": f"Test message requested by {user.email}.",
            "severity": "info",
            "device_hostname": None,
            "alert_id": None,
            "context": {"channel": channel.name},
        },
    )
    db.add(entry)
    await db.commit()
    return OutboxOut.model_validate(entry)


@router.get("/outbox")
async def list_outbox(
    db: DbSession, _user: CurrentUser, limit: int = Query(default=50, ge=1, le=200)
) -> list[OutboxOut]:
    rows = await db.execute(
        select(NotificationOutbox).order_by(NotificationOutbox.created_at.desc()).limit(limit)
    )
    return [OutboxOut.model_validate(o) for o in rows.scalars()]


@router.get("/outbox/health")
async def outbox_health_view(db: DbSession, _user: CurrentUser) -> dict:
    """Queue depth, oldest undelivered age, blocked and recently-failed counts.

    A notification that never arrives leaves no trace anywhere else in the UI.
    The same numbers are available to /health/ingest via
    openrmm.services.outbox.outbox_health.
    """
    return await outbox_health(db)
