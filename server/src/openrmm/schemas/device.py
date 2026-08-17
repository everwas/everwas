import uuid
from datetime import datetime

from pydantic import BaseModel, Field, HttpUrl, model_validator

from openrmm.models.device import DeviceStatus, OsFamily


class DeviceOut(BaseModel):
    id: uuid.UUID
    hostname: str
    os_family: OsFamily
    os_version: str
    arch: str
    agent_version: str
    status: DeviceStatus
    tags: list[str]
    last_heartbeat_at: datetime | None
    enrolled_at: datetime

    model_config = {"from_attributes": True}


class DeviceDetailOut(DeviceOut):
    cpu_pct: float | None = None
    mem_pct: float | None = None
    worst_disk_pct: float | None = None


class TelemetryPoint(BaseModel):
    ts: datetime
    cpu_pct: float | None
    mem_pct: float | None
    load1: float | None


class NetRatePoint(BaseModel):
    """One interface sample, as per-second rates.

    Every field is nullable and a null is meaningful: it marks a point where
    the rate is genuinely unknown (the counter reset, or the agent was away
    long enough that an average would lie) rather than a measured zero. Chart
    it as a break in the line, never as 0 and never interpolated.
    """

    ts: datetime
    bytes_sent: float | None = None
    bytes_recv: float | None = None
    packets_sent: float | None = None
    packets_recv: float | None = None
    err_in: float | None = None
    err_out: float | None = None
    drop_in: float | None = None
    drop_out: float | None = None


class NetInterfaceSeries(BaseModel):
    name: str
    # Carried from inventory so the chart can label a NIC with what it is,
    # not just what the kernel calls it.
    mac: str | None = None
    up: bool | None = None
    addresses: list[str] = []
    points: list[NetRatePoint] = []


class FactOut(BaseModel):
    fact_key: str
    payload: dict
    valid_from: datetime | None
    valid_to: datetime | None
    source: str


class AgentUpdateRequest(BaseModel):
    """A self-update instruction, forwarded to the agent verbatim.

    There is no default artifact URL and no way to skip the signature. An
    update that could be dispatched with a URL alone would make "run arbitrary
    code as root on every machine" a one-field request, and the agent's
    minisign check is the only thing standing between a compromised artifact
    host and the whole fleet. The agent refuses an unsigned request anyway;
    requiring it here means the refusal is a 422 to the operator instead of a
    job that fails on every host a minute later.
    """

    version: str = Field(min_length=1, max_length=64)
    artifact_url: HttpUrl
    sha256: str = Field(pattern=r"^[0-9a-fA-F]{64}$")

    # Exactly one of these carries the minisign signature. The agent fetches
    # signature_url itself when the body is not inlined.
    signature: str | None = None
    signature_url: HttpUrl | None = None

    # Signing keys rotated in since the agent enrolled. The agent trusts these
    # IN ADDITION to its embedded key, never instead of it, so this field
    # cannot be used to swap the trust anchor.
    public_keys: list[str] | None = None

    # Re-apply a version this host already rolled back. An operator decision,
    # so it has to be said out loud rather than inferred from a retry.
    force: bool = False

    @model_validator(mode="after")
    def _needs_a_signature(self) -> "AgentUpdateRequest":
        if not self.signature and not self.signature_url:
            raise ValueError("one of signature or signature_url is required")
        return self
