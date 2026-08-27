from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="EVERWAS_", env_file=".env", extra="ignore")

    mode: str = "dev"
    secret_key: str = "dev-only-insecure"
    database_url: str = "postgresql+asyncpg://everwas:everwas@localhost:5432/everwas"
    nats_url: str = "nats://localhost:4222"
    # what enrolled agents are told to dial (wss:// behind Caddy in prod)
    nats_public_url: str = "nats://localhost:4222"
    nats_auth_seed: str = ""
    # credentials the api/dispatcher use on the internal NATS connection
    # (listed in nats.conf auth_users, so they bypass the callout)
    nats_server_user: str = "server"
    nats_server_password: str = ""
    # How long an agent's issued user JWT is good for. NATS runs auth-callout
    # at CONNECT time only, so this is the revocation latency: a retired agent
    # keeps its session until the JWT expires. Without an expiry, retiring a
    # device does nothing at all to a machine that is already connected.
    # nats.go reconnects on its own when the server closes an expired session.
    nats_jwt_ttl_s: int = 3600

    session_ttl_hours: int = 24 * 7
    # Lifetime of the bearer tokens the sync API mints from an API key
    # (POST /api/v1/auth/token). Revoking the key invalidates its tokens
    # immediately regardless of this value; the TTL only bounds how long a
    # captured token works on its own.
    sync_token_ttl_s: int = 3600
    # Ceiling on how many machines one script run or patch deploy may touch.
    # A fleet-wide selector is a decision, not a default.
    max_run_targets: int = 500
    heartbeat_offline_after_s: int = 90
    telemetry_retention_days: int = 30
    recordings_dir: str = "/data/recordings"

    #: Where agent installers live, to be served to the fleet. Hosting them on
    #: the server the agent enrols to is what makes bootstrapping work from a
    #: provisioning or quarantine VLAN, where the management server is often
    #: the one host a device is allowed to reach.
    packages_dir: str = "/data/packages"

    #: Where the device-issuing CA lives. The intermediate signing key is
    #: encrypted with ca_passphrase; the root private key is never stored here
    #: (see services/ca.py).
    ca_dir: str = "/data/ca"
    #: Unlocks the intermediate signing key. Empty means certificate issuance
    #: is switched off, which is the correct default: a CA that springs into
    #: existence with a passphrase nobody chose is a CA nobody is guarding.
    ca_passphrase: str = ""
    #: How long an issued device certificate is valid. The agent renews at half
    #: of whatever it was issued, reading the window from the certificate
    #: itself, so changing this needs no agent change.
    #:
    #: 90 days is the value for a deployment WITHOUT a remediation VLAN, where
    #: an expired certificate means the machine cannot reach the network to be
    #: fixed and recovery is a physical visit. Once 802.1X failure lands a
    #: device somewhere it can still reach this server (ADR-0004), expiry stops
    #: being a truck roll and this should drop to 30: a superseded certificate
    #: then retires itself within a month, with no CRL and none of the
    #: fleet-wide failure modes CRL publication carries.
    #:
    #: Do NOT shorten it before that recovery path exists. The floor is not the
    #: holiday laptop, which remediation recovers; it is how long THIS server
    #: may be unavailable, since renewal at half life is the entire margin.
    ca_cert_lifetime_days: int = 90

    smtp_host: str = ""
    smtp_port: int = 587
    smtp_user: str = ""
    smtp_password: str = ""
    smtp_from: str = "everwas@localhost"

    mcp_enabled: bool = False

    #: NATS subject each device's posture collection is pushed to for an
    #: access verifier (l2trace consumes `l2trace.posture`). Empty disables
    #: egress entirely, which is the safe default: a verifier treats absence
    #: as not-assessed and not-assessed never gates, so a deployment without
    #: one loses nothing by publishing nothing. Published on the server's
    #: existing NATS connection; a publish failure never fails ingest (see
    #: everwas/egress/posture.py).
    posture_egress_subject: str = ""


@lru_cache
def get_settings() -> Settings:
    return Settings()
