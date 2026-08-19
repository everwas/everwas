"""Fleet simulator: enroll N fake devices and pump telemetry + inventory
through NATS to exercise ingest at scale (M2 DoD: 500 devices).

Publishes as the internal `server` user (single connection) — this tests the
ingest path and query plans, not the auth callout (the stub agent covers that).
"""

import asyncio
import json
import random
import time
import uuid

import nats
import structlog
import typer

from everwas.config import get_settings
from everwas.db.engine import session_scope
from everwas.models.device import Device, DeviceStatus, OsFamily
from everwas.util.ids import uuid7

log = structlog.get_logger()
cli = typer.Typer()

ADJECTIVES = ["red", "blue", "fast", "calm", "bold", "gray", "warm", "cool"]
NOUNS = ["falcon", "otter", "maple", "cedar", "raven", "lynx", "heron", "moose"]


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


def _telemetry(rng: random.Random) -> dict:
    total = 16 * 2**30
    return {
        "cpu_pct": round(rng.uniform(1, 95), 1),
        "mem_used": int(total * rng.uniform(0.2, 0.9)),
        "mem_total": total,
        "swap_pct": round(rng.uniform(0, 10), 1),
        "load1": round(rng.uniform(0, 4), 2),
        "uptime_s": rng.randint(3600, 90 * 86400),
        "disks": [
            {
                "mount": "/",
                "used": int(500 * 2**30 * rng.uniform(0.3, 0.95)),
                "total": 500 * 2**30,
                "fstype": "ext4",
            },
        ],
    }


def _software(rng: random.Random) -> dict:
    packages = [{"name": f"pkg-{i}", "version": f"1.{rng.randint(0, 3)}"} for i in range(120)]
    return {"packages": packages}


async def _run(count: int, rounds: int) -> None:
    settings = get_settings()

    async with session_scope() as db:
        device_ids: list[str] = []
        for i in range(count):
            name = f"{random.choice(ADJECTIVES)}-{random.choice(NOUNS)}-{i:03d}"
            device = Device(
                id=uuid7(),
                hostname=name,
                os_family=random.choice(list(OsFamily)),
                os_version="sim",
                arch="x86_64",
                agent_version="sim-0",
                status=DeviceStatus.enrolled,
            )
            db.add(device)
            device_ids.append(str(device.id))
    log.info("devices enrolled", count=count)

    nc = await nats.connect(
        settings.nats_url,
        user=settings.nats_server_user,
        password=settings.nats_server_password,
        name="fleet-sim",
    )

    for round_no in range(rounds):
        for device_id in device_ids:
            rng = random.Random(f"{device_id}:{round_no}")
            await nc.publish(
                f"agents.{device_id}.heartbeat",
                _envelope("heartbeat", device_id, {"version": "sim-0", "seq": round_no}),
            )
            await nc.publish(
                f"agents.{device_id}.telemetry", _envelope("telemetry", device_id, _telemetry(rng))
            )
            if round_no == 0:
                await nc.publish(
                    f"agents.{device_id}.inventory.software",
                    _envelope("inventory", device_id, _software(rng)),
                )
        await nc.flush()
        log.info("round published", round=round_no + 1, of=rounds)
        if round_no < rounds - 1:
            await asyncio.sleep(2)

    await nc.drain()
    log.info("fleet sim complete", devices=count, rounds=rounds)


@cli.command()
def main(count: int = 500, rounds: int = 3) -> None:
    asyncio.run(_run(count, rounds))


if __name__ == "__main__":
    cli()
