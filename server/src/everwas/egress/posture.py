"""Push each device's posture collection to an access verifier (l2trace).

The wire contract is settled on the posture-verdict-model thread and the
verifier has built its ingress against it: ONE envelope per device per
collection, carrying the per-check results exactly as the agent serialised
them. `status` is three values (`pass` / `fail` / `not_assessed`) with the
not-applicable versus undetermined distinction in `not_assessed_reason`;
`category`, `detail`, `evidence` and `took_ms` ride along untouched. Nothing
here re-derives or collapses a check: the agent's MarshalJSON is the
serialisation choke point that produced the safe three-state form, and this
module's job is to not undo that.

The envelope adds what only the server knows:

- `device_id`, the identity the verifier keys its cache on (it is also the
  certificate CN on the EAP-TLS path),
- `macs`, the current MAC set from the network facts with loopbacks
  excluded. For a device that authenticates by MAB this is the ONLY join
  material the verifier has, so it travels on every envelope,
- `ingested_at`, THIS server's clock. Freshness gates on this, never on the
  endpoint's clock, and
- `collected_at`, the endpoint-reported time, carried alongside as forensic
  context because an endpoint's own clock is occasionally the only way to
  explain a weird result.

Publishing rides the ingest of a posture collection, so the cadence is the
agent's collection interval: 30 minutes today. The verifier's freshness
window is 2 hours -- three intervals -- and was chosen against that number.
If the cadence ever changes, tell them on the thread before shipping it.

Off by default: with no EVERWAS_POSTURE_EGRESS_SUBJECT there is no
publisher, and ingest proceeds exactly as before.
"""

import json
import uuid
from datetime import UTC, datetime

import structlog
from sqlalchemy import select

from everwas.bitemporal.query import get_facts
from everwas.db.engine import session_scope
from everwas.models.device import Device

log = structlog.get_logger()


def current_macs(network_facts: list[dict]) -> list[str]:
    """The device's current MAC set, from its per-interface network facts.

    Loopbacks are excluded; interfaces without a hardware address (tunnels,
    some virtual adapters) contribute nothing. Deduplicated and sorted so
    the set is stable across collections: a bridge sharing its member's MAC
    must not make the same address appear twice, and ordering churn must not
    look like a change to a consumer diffing envelopes.
    """
    macs = set()
    for fact in network_facts:
        if not fact["fact_key"].startswith("iface:"):
            continue
        payload = fact["payload"]
        if payload.get("loopback"):
            continue
        mac = payload.get("mac")
        if mac:
            macs.add(mac)
    return sorted(macs)


def _utc_z(dt: datetime) -> str:
    return dt.astimezone(UTC).isoformat().replace("+00:00", "Z")


def build_envelope(
    device: Device,
    network_facts: list[dict],
    checks: list[dict],
    *,
    collected_at: datetime,
    ingested_at: datetime,
) -> dict:
    return {
        "device_id": str(device.id),
        "hostname": device.hostname,
        "agent_version": device.agent_version,
        "macs": current_macs(network_facts),
        # Endpoint clock. Forensic only; it must never gate freshness,
        # because an endpoint with a skewed clock would read permanently
        # stale (or permanently fresh) no matter what it reports.
        "collected_at": _utc_z(collected_at),
        # Our clock, stamped here. This is the field the verifier's
        # freshness window is measured against.
        "ingested_at": _utc_z(ingested_at),
        # Passed through exactly as the agent serialised them.
        "checks": checks,
    }


class PosturePublisher:
    """Builds and publishes one envelope per ingested posture collection."""

    def __init__(self, nc, subject: str) -> None:
        self._nc = nc
        self._subject = subject

    async def publish(self, device_id: uuid.UUID, collected_at: datetime, payload: dict) -> None:
        async with session_scope() as db:
            device = (
                await db.execute(select(Device).where(Device.id == device_id))
            ).scalar_one_or_none()
            if device is None:
                log.warning("posture egress skipped: unknown device", device_id=str(device_id))
                return
            network_facts = await get_facts(db, "network", device_id)

        envelope = build_envelope(
            device,
            network_facts,
            payload.get("checks") or [],
            collected_at=collected_at,
            ingested_at=datetime.now(UTC),
        )
        await self._nc.publish(self._subject, json.dumps(envelope).encode())
        log.info(
            "posture egress published",
            device_id=str(device_id),
            subject=self._subject,
            checks=len(envelope["checks"]),
        )


_PUBLISHER: PosturePublisher | None = None


def set_publisher(publisher: PosturePublisher | None) -> None:
    """Install the process-wide publisher. None (the default) disables egress."""
    global _PUBLISHER
    _PUBLISHER = publisher


async def publish_posture(device_id: uuid.UUID, collected_at: datetime, payload: dict) -> None:
    """Publish one device's just-ingested posture collection. Never raises.

    Called from the inventory ingest handler AFTER the facts committed.
    """
    publisher = _PUBLISHER
    if publisher is None:
        return
    try:
        await publisher.publish(device_id, collected_at, payload)
    except Exception:
        # Swallowed on purpose, and the reasoning matters more than the
        # except: a publish failure must never fail the ingest it rides on.
        # The facts are already committed, so raising here would nak and
        # redeliver a message whose storage succeeded, re-amending nothing
        # and eventually dead-lettering a perfectly good collection because
        # an EGRESS peer was down. There is deliberately no retry queue in
        # v1 either: the next collection (30 minutes) is the retry, and the
        # verifier reads a stale assessment as not-assessed, which never
        # gates. A dropped envelope costs a briefly staler cache on their
        # side; "fixing" this into a raise costs ingest its independence
        # from a consumer we do not operate.
        log.exception("posture egress failed", device_id=str(device_id))
