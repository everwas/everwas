"""Stand-in agent for server development until the Go agent lands (M1 DoD tool).

Enrolls over HTTP, connects to NATS with the issued per-agent credentials,
publishes heartbeats, and (with --test-foreign) proves the auth callout pins
subjects by attempting a publish into another agent's namespace.
"""

import asyncio
import json
import platform
import socket
import time
import uuid

import httpx
import nats
import structlog
import typer

log = structlog.get_logger()
cli = typer.Typer()

HEARTBEAT_INTERVAL_S = 15


def _envelope(msg_type: str, agent_id: str, data: dict) -> bytes:
    return json.dumps(
        {
            "v": 1,
            "type": msg_type,
            "agent_id": agent_id,
            "msg_id": uuid.uuid4().hex,
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "data": data,
        }
    ).encode()


async def _run(server: str, token: str, nats_url_override: str | None, test_foreign: bool) -> None:
    os_family = {"Linux": "linux", "Darwin": "macos", "Windows": "windows"}[platform.system()]
    async with httpx.AsyncClient(base_url=server) as http:
        resp = await http.post(
            "/api/v1/agents/enroll",
            json={
                "token": token,
                "hostname": socket.gethostname(),
                "os_family": os_family,
                "os_version": platform.release(),
                "arch": platform.machine(),
                "agent_version": "stub-0",
            },
        )
        resp.raise_for_status()
        cred = resp.json()

    agent_id = cred["agent_id"]
    nats_url = nats_url_override or cred["nats_url"]
    log.info("enrolled", agent_id=agent_id, nats_url=nats_url)

    errors: list[str] = []

    async def error_cb(e: Exception) -> None:
        errors.append(str(e))

    nc = await nats.connect(
        nats_url,
        name=f"stub-agent-{agent_id[:8]}",
        user=agent_id,
        password=cred["agent_secret"],
        max_reconnect_attempts=-1,
        error_cb=error_cb,
    )
    log.info("connected to nats")

    if test_foreign:
        foreign = f"agents.{uuid.uuid4()}.heartbeat"
        await nc.publish(foreign, _envelope("heartbeat", agent_id, {}))
        await nc.flush()
        await asyncio.sleep(0.5)
        if any("permissions violation" in e.lower() for e in errors):
            log.info("conformance OK: foreign-subject publish refused", subject=foreign)
        else:
            log.error("conformance FAILED: foreign publish not refused!", errors=errors)

    seq = 0
    while True:
        seq += 1
        await nc.publish(
            f"agents.{agent_id}.heartbeat",
            _envelope("heartbeat", agent_id, {"version": "stub-0", "seq": seq}),
        )
        log.info("heartbeat sent", seq=seq)
        await asyncio.sleep(HEARTBEAT_INTERVAL_S)


@cli.command()
def main(
    server: str = "http://localhost:28000",
    token: str = typer.Option(..., help="enrollment token (ore_...)"),
    nats_url: str = typer.Option(None, help="override the NATS URL from enrollment"),
    test_foreign: bool = typer.Option(
        False, help="attempt a foreign-subject publish (conformance)"
    ),
) -> None:
    asyncio.run(_run(server, token, nats_url, test_foreign))


if __name__ == "__main__":
    cli()
