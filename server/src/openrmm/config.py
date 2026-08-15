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

    session_ttl_hours: int = 24 * 7
    heartbeat_offline_after_s: int = 90
    telemetry_retention_days: int = 30

    smtp_host: str = ""
    smtp_port: int = 587
    smtp_user: str = ""
    smtp_password: str = ""
    smtp_from: str = "openrmm@localhost"

    mcp_enabled: bool = False


@lru_cache
def get_settings() -> Settings:
    return Settings()
