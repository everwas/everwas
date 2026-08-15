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


def main() -> None:
    cli()
