"""NATS auth-callout responder.

Every non-auth_users connection to NATS lands here as a request on
$SYS.REQ.USER.AUTH. We verify the presented user/pass against
agent_credentials and answer with a user JWT pinned to that agent's subjects
(docs/nats-subjects.md), or an error JWT to deny.
"""

import time
import uuid

import nats
import nkeys
import structlog

from openrmm.config import Settings
from openrmm.db.engine import session_scope
from openrmm.natsio.jwt import decode_jwt_payload, encode_nats_jwt, new_jti
from openrmm.natsio.subjects import agent_permissions
from openrmm.services.enrollment import verify_agent_secret

log = structlog.get_logger()

AUTH_CALLOUT_SUBJECT = "$SYS.REQ.USER.AUTH"
GLOBAL_ACCOUNT = "$G"


def _issuer_public(seed: str) -> str:
    kp = nkeys.from_seed(seed.encode())
    try:
        return kp.public_key.decode()
    finally:
        kp.wipe()


def _user_jwt(issuer: str, seed: str, user_nkey: str, agent_id: str) -> str:
    perms = agent_permissions(agent_id)
    return encode_nats_jwt(
        {
            "jti": new_jti(),
            "iat": int(time.time()),
            "iss": issuer,
            "sub": user_nkey,
            "aud": GLOBAL_ACCOUNT,
            "name": f"agent-{agent_id[:8]}",
            "nats": {
                "type": "user",
                "version": 2,
                "pub": {"allow": perms["publish"]},
                "sub": {"allow": perms["subscribe"]},
            },
        },
        seed,
    )


def _response_jwt(
    issuer: str, seed: str, user_nkey: str, server_id: str, *, jwt: str = "", error: str = ""
) -> bytes:
    nats_claim: dict = {"type": "authorization_response", "version": 2}
    # Exactly one of jwt/error may be set (server rejects both or neither).
    if jwt:
        nats_claim["jwt"] = jwt
    else:
        nats_claim["error"] = error or "authorization denied"
    return encode_nats_jwt(
        {
            "jti": new_jti(),
            "iat": int(time.time()),
            "iss": issuer,
            "sub": user_nkey,
            "aud": server_id,
            "nats": nats_claim,
        },
        seed,
    ).encode()


async def _authorize(settings: Settings, connect_user: str, connect_pass: str) -> str | None:
    """Return the agent_id if credentials check out, else None."""
    try:
        agent_id = uuid.UUID(connect_user)
    except ValueError:
        return None
    async with session_scope() as db:
        ok = await verify_agent_secret(db, agent_id, connect_pass)
    return str(agent_id) if ok else None


async def auth_callout_responder(nc: nats.NATS, settings: Settings) -> None:
    seed = settings.nats_auth_seed
    issuer = _issuer_public(seed)
    sub = await nc.subscribe(AUTH_CALLOUT_SUBJECT, queue="auth-callout")
    log.info("auth callout responder up", issuer=issuer)

    async for msg in sub.messages:
        try:
            req = decode_jwt_payload(msg.data)["nats"]
            user_nkey = req["user_nkey"]
            server_id = req["server_id"]["id"]
            opts = req.get("connect_opts") or {}
            agent_id = await _authorize(settings, opts.get("user") or "", opts.get("pass") or "")
            if agent_id is not None:
                user_jwt = _user_jwt(issuer, seed, user_nkey, agent_id)
                await msg.respond(_response_jwt(issuer, seed, user_nkey, server_id, jwt=user_jwt))
                log.info("agent authorized", agent_id=agent_id)
            else:
                await msg.respond(
                    _response_jwt(
                        issuer, seed, user_nkey, server_id, error="invalid agent credentials"
                    )
                )
                log.warning("agent authorization refused", user=opts.get("user"))
        except Exception:
            log.exception("auth callout request failed")
