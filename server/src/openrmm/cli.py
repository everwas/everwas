import asyncio
import getpass

import typer

from openrmm import __version__

cli = typer.Typer(help="OpenRMM server administration", no_args_is_help=True)


@cli.command()
def version() -> None:
    typer.echo(__version__)


@cli.command()
def create_admin(email: str, password: str | None = None) -> None:
    """Create (or promote) an admin user."""
    from sqlalchemy import select

    from openrmm.db.engine import session_scope
    from openrmm.models.user import Role, User
    from openrmm.security.passwords import hash_password

    if password is None:
        password = getpass.getpass("Password: ")

    async def run() -> str:
        async with session_scope() as db:
            row = await db.execute(select(User).where(User.email == email.lower()))
            user = row.scalar_one_or_none()
            if user is None:
                db.add(
                    User(
                        email=email.lower(),
                        password_hash=hash_password(password),
                        role=Role.admin,
                    )
                )
                return "created"
            user.role = Role.admin
            user.password_hash = hash_password(password)
            return "updated"

    typer.echo(f"admin {email}: {asyncio.run(run())}")


@cli.command()
def gen_enrollment_token(max_uses: int = 1, ttl_hours: int = 24, created_by: str = "cli") -> None:
    """Mint a one-time (by default) agent enrollment token. Shown once."""
    from openrmm.db.engine import session_scope
    from openrmm.services.enrollment import mint_enrollment_token

    async def run() -> str:
        async with session_scope() as db:
            _, token = await mint_enrollment_token(
                db, max_uses=max_uses, ttl_hours=ttl_hours, created_by=created_by
            )
            return token

    typer.echo(asyncio.run(run()))


@cli.command()
def create_api_key(
    name: str,
    scopes: str = "devices:read,alerts:read,patches:read",
    ttl_days: int = 0,
) -> None:
    """Mint an API key for automation or the MCP server. Shown once."""
    import hashlib
    import secrets as _secrets
    from datetime import UTC, datetime, timedelta

    from openrmm.db.engine import session_scope
    from openrmm.models.api_key import ApiKey

    async def run() -> str:
        key_id = _secrets.token_hex(11)  # 22 chars
        secret = _secrets.token_urlsafe(32)
        async with session_scope() as db:
            db.add(
                ApiKey(
                    name=name,
                    key_id=key_id,
                    secret_hash=hashlib.sha256(secret.encode()).hexdigest(),
                    scopes=[s.strip() for s in scopes.split(",") if s.strip()],
                    expires_at=(datetime.now(UTC) + timedelta(days=ttl_days) if ttl_days else None),
                )
            )
        return f"orpk_{key_id}_{secret}"

    typer.echo(asyncio.run(run()))


@cli.command()
def gen_nats_keys() -> None:
    """Generate the auth-callout account keypair. Put both values in .env."""
    from openrmm.natsio.jwt import generate_account_keypair

    seed, public = generate_account_keypair()
    typer.echo(f"OPENRMM_NATS_AUTH_SEED={seed}")
    typer.echo(f"OPENRMM_NATS_AUTH_ISSUER={public}")


def main() -> None:
    cli()
