from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="OPENRMM_", env_file=".env", extra="ignore")

    mode: str = "dev"
    secret_key: str = "dev-only-insecure"
    database_url: str = "postgresql+asyncpg://openrmm:openrmm@localhost:5432/openrmm"
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

    smtp_host: str = ""
    smtp_port: int = 587
    smtp_user: str = ""
    smtp_password: str = ""
    smtp_from: str = "openrmm@localhost"

    mcp_enabled: bool = False


@lru_cache
def get_settings() -> Settings:
    return Settings()
