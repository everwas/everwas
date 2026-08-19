"""The Everwas MCP server.

Transport is streamable HTTP at /mcp. Authentication is an Everwas API key
presented as a bearer token, verified against the api_keys table on every
request, so an unauthenticated call never reaches a tool.
"""

import asyncio
import os
import sys
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import nats
import structlog
from fastmcp import FastMCP
from fastmcp.server.auth.auth import AccessToken, TokenVerifier

from everwas import __version__
from everwas.config import get_settings
from everwas.mcp.context import access_token_for, authenticate
from everwas.mcp.tools import register_tools

log = structlog.get_logger()

MCP_HOST = os.environ.get("EVERWAS_MCP_HOST", "0.0.0.0")
MCP_PORT = int(os.environ.get("EVERWAS_MCP_PORT", "8001"))
MCP_PATH = os.environ.get("EVERWAS_MCP_PATH", "/mcp")
NATS_CONNECT_TIMEOUT_S = float(os.environ.get("EVERWAS_MCP_NATS_TIMEOUT_S", "10"))

INSTRUCTIONS = """\
Everwas manages a fleet of enrolled machines (Windows, macOS, Linux), each
running an agent that reports heartbeats, telemetry, inventory, and patch
state. Use these tools to answer questions about that fleet and, with the
user's explicit confirmation, to act on it.

Inventory is stored bitemporally, which means two different time questions
have two different parameters, and mixing them up gives a confidently wrong
answer:

- as_of is VALID time, when something was true on the machine.
  "What was installed on web-01 last Tuesday?" -> as_of=last Tuesday.
- knew_at is RECORD time, when the server believed it.
  "What did we think was installed on web-01, going on the reports we had by
  last Tuesday?" -> knew_at=last Tuesday.

They differ whenever an agent reports late or a later scan corrects an earlier
belief, which is exactly the case that matters after an incident. Both take
ISO-8601 timestamps and both require a timezone.

Safety rules built into this server, which you should also state plainly to
the user:

- Every mutating tool (acknowledge_alert, run_script, approve_patches) takes
  confirm and defaults to false. A call with confirm=false changes nothing and
  returns the plan. Show the plan, get a yes, then confirm.
- Access is scoped per API key. A refusal means the key lacks a scope, not
  that you asked wrongly; say so instead of retrying.
- Every call, successful or refused, is written to the Everwas audit log under
  the API key's name. Assume the user will read it.
"""


class ApiKeyVerifier(TokenVerifier):
    """Verifies `ewpk_...` bearer tokens against the api_keys table.

    FastMCP rejects the request before any tool runs when this returns None,
    which is the fail-closed path for HTTP transport.
    """

    async def verify_token(self, token: str) -> AccessToken | None:
        try:
            principal = await authenticate(token)
        except Exception as exc:  # noqa: BLE001 - a lookup we cannot complete is a denial
            log.error("mcp auth lookup failed", error=str(exc))
            return None
        if principal is None:
            log.info("mcp auth rejected")
            return None
        return access_token_for(principal, token)


async def _connect_nats() -> nats.NATS | None:
    """Own connection for this process, named so it is obvious on the NATS console."""
    settings = get_settings()
    try:
        nc = await asyncio.wait_for(
            nats.connect(
                settings.nats_url,
                name="everwas-mcp",
                user=settings.nats_server_user,
                password=settings.nats_server_password,
                max_reconnect_attempts=-1,
            ),
            timeout=NATS_CONNECT_TIMEOUT_S,
        )
    except Exception as exc:  # noqa: BLE001 - degrade to read-only rather than refuse to boot
        log.warning("mcp: nats unavailable, action tools will refuse", error=str(exc))
        return None
    log.info("mcp connected to nats")
    return nc


@asynccontextmanager
async def lifespan(server: FastMCP) -> AsyncIterator[dict]:
    """Hold the process's NATS connection for the duration of the server.

    The dial only happens when the MCP server is actually enabled, so importing
    this module (in tests, in `fastmcp inspect`) never touches the network.
    """
    nc = await _connect_nats() if get_settings().mcp_enabled else None
    try:
        yield {"nats": nc}
    finally:
        if nc is not None:
            await nc.drain()


mcp = FastMCP(
    "everwas",
    instructions=INSTRUCTIONS,
    version=__version__,
    auth=ApiKeyVerifier(),
    lifespan=lifespan,
)
register_tools(mcp)


def main() -> None:
    settings = get_settings()
    if not settings.mcp_enabled:
        print(
            "everwas-mcp: refusing to start because EVERWAS_MCP_ENABLED is not true.\n"
            "The MCP server is opt-in: set EVERWAS_MCP_ENABLED=true in .env and run\n"
            "  docker compose --profile mcp up -d everwas-mcp",
            file=sys.stderr,
        )
        raise SystemExit(1)

    # stderr only: stdout belongs to the transport
    print(
        f"everwas-mcp v{__version__} on http://{MCP_HOST}:{MCP_PORT}{MCP_PATH} "
        "(bearer: an Everwas API key, ewpk_...)",
        file=sys.stderr,
    )
    mcp.run(
        transport="http",
        host=MCP_HOST,
        port=MCP_PORT,
        path=MCP_PATH,
        stateless_http=True,
        json_response=True,
    )


if __name__ == "__main__":
    main()
