"""Response shapes for /api/v1/sync — the integration-facing read surface.

These are a contract with external consumers (docs/sync-api.md), not a
mirror of the SPA's schemas: the SPA can change its payloads with its own
frontend, the sync shapes only additively. Field-level rules that matter:

- Nullable identity fields mean "no agent has asserted this yet" (old agent,
  no DMI tables), never "empty". Consumers should treat null as unknown.
- ip_addresses / interface addresses stay in CIDR form as the agent reported
  them: the prefix length is exactly what an IPAM consumer needs and cannot
  reconstruct once stripped.
- Concrete page models per item type rather than a generic, so the OpenAPI
  schema names are stable and readable for consumers generating clients.
"""

import uuid
from datetime import datetime

from pydantic import BaseModel


class SyncPageBase(BaseModel):
    has_more: bool
    next_cursor: str | None


class SyncOrgOut(BaseModel):
    id: uuid.UUID
    name: str
    description: str | None


class SyncOrgPage(SyncPageBase):
    items: list[SyncOrgOut]


class SyncSiteOut(BaseModel):
    id: uuid.UUID
    org_id: uuid.UUID
    name: str
    description: str | None
    address: str | None
    created_at: datetime


class SyncSitePage(SyncPageBase):
    items: list[SyncSiteOut]


class SyncDeviceOut(BaseModel):
    # From the device row
    id: uuid.UUID
    org_id: uuid.UUID
    site_id: uuid.UUID | None
    hostname: str
    #: The raw device state (enrolled|active|offline|retired). Kept for
    #: convenience, but it conflates a durable lifecycle fact with 90-second
    #: reachability telemetry — consumers should key policy on `lifecycle`
    #: and treat `status`/`reachable` as volatile.
    status: str
    #: The durable half of status: enrolled (never heartbeated) |
    #: operational | retired. Safe to diff; changes mean something.
    lifecycle: str
    #: The volatile half: last heartbeat within the offline threshold. Null
    #: when the device has never heartbeated. Telemetry, not inventory —
    #: exclude from diffs.
    reachable: bool | None
    tags: list[str]
    agent_version: str
    os_family: str
    os_version: str
    arch: str
    enrolled_at: datetime
    last_heartbeat_at: datetime | None

    # From the current hardware facts (null until an agent reports them)
    manufacturer: str | None
    model: str | None
    serial_number: str | None
    chassis_type: str | None
    cpu_model: str | None
    cpu_cores: int | None
    memory_bytes: int | None
    is_virtual: bool | None

    #: "vm" when virtual, else the chassis bucket, else null. The one field
    #: a consumer can route device-class decisions on without re-deriving.
    device_class: str | None

    #: Always null today: OpenRMM does not track DNS names. Present so the
    #: field's absence is a documented statement rather than a missing key.
    dns_name: str | None

    # Rolled up from current network facts, loopbacks excluded
    mac_addresses: list[str]
    ip_addresses: list[str]


class SyncDevicePage(SyncPageBase):
    items: list[SyncDeviceOut]


class SyncInterfaceOut(BaseModel):
    device_id: uuid.UUID
    #: The stable per-device key (the interface name; fact key minus its
    #: prefix). Unique per device under the current-belief predicate.
    key: str
    name: str
    mac: str | None
    mtu: int | None
    up: bool | None
    loopback: bool
    addresses: list[str]
    #: lower(valid_during): when the machine last reported this state.
    observed_at: datetime


class SyncInterfacePage(SyncPageBase):
    items: list[SyncInterfaceOut]


class SyncSoftwareOut(BaseModel):
    device_id: uuid.UUID
    name: str
    version: str
    observed_at: datetime


class SyncSoftwarePage(SyncPageBase):
    items: list[SyncSoftwareOut]


class SyncPostureOut(BaseModel):
    device_id: uuid.UUID
    #: The check's stable name (fact key minus its prefix) — the per-device
    #: natural key. Per-check rather than a rollup: the check set grows over
    #: time, and a check ABSENT from a device's rows never ran there, which
    #: is not a failure and must not read as one.
    check: str
    #: Agent-defined vocabulary: pass | fail | not_applicable today, and new
    #: values may appear before consumers learn them. Treat an unknown status
    #: as not-assessed, never as failed — only an explicit "fail" is a
    #: failure. That is the agreed contract with the l2trace quarantine
    #: integration, where misreading "not assessed" cuts a machine off the
    #: network.
    status: str
    #: Human-oriented explanation. "" means the agent gave none — the verdict
    #: stands on its own, and empty is not missing here.
    detail: str
    #: lower(valid_during): when the machine last reported this verdict.
    observed_at: datetime


class SyncPosturePage(SyncPageBase):
    items: list[SyncPostureOut]


class SyncPatchOut(BaseModel):
    device_id: uuid.UUID
    external_id: str
    #: The canonical key for a patch catalog: the first KB id when the patch
    #: has one, else external_id. Server-computed so every consumer inherits
    #: the same precedence instead of inventing it.
    identifier: str
    title: str
    kind: str
    severity: str
    kb_ids: list[str]
    cves: list[str]
    size_bytes: int | None
    reboot_likely: bool
    #: approved | declined | pending — the operator's standing decision, not
    #: install progress. Device-specific approvals shadow fleet-wide ones.
    status: str
    unsupported: bool
    detail: str
    observed_at: datetime
    first_seen_at: datetime | None


class SyncPatchPage(SyncPageBase):
    items: list[SyncPatchOut]


class SyncChangeOut(BaseModel):
    device_id: uuid.UUID
    kind: str
    fact_key: str
    payload: dict
    #: recorded — the server started believing this; superseded — it stopped
    #: (an amend closed it: the value changed, or the fact disappeared).
    #: A disappearance produces BOTH: the open belief is superseded and a
    #: tombstone is recorded with valid_to set ("it WAS there until T").
    #: A fact is current only while its latest recorded event has
    #: valid_to == null.
    change: str
    #: When the belief window opened (recorded) or closed (superseded), i.e.
    #: record time. This is what `since` filters on and what orders the feed.
    at: datetime
    valid_from: datetime | None
    valid_to: datetime | None


class SyncChangePage(SyncPageBase):
    items: list[SyncChangeOut]
