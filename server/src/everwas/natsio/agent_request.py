"""Request/reply to an agent, over a reply subject the agent may publish to.

`nc.request()` cannot be used here. It replies on the client's own inbox
prefix, a shared `_INBOX.…` in the account, and the agent's publish grant is
pinned to its own namespace precisely so it cannot touch anything shared. The
agent therefore receives the request, does the work, and is REFUSED when it
tries to answer:

    Publish Violation - Subject "_INBOX.GyySbggx…"

which the server sees only as a timeout. That combination is the worst of both
worlds: the command ran, the caller was told it failed, and nothing in either
log says the two facts are related. It bit credential rotation for real, where
"failed" plus "actually applied" is how an agent gets locked out.

The fix is to reply inside the agent's own inbox namespace, which it is
already granted. Nothing about the grants has to be widened.
"""

import secrets

import nats
import nats.errors
import structlog

from everwas.natsio.subjects import agent_inbox_prefix, cmd

log = structlog.get_logger()

DEFAULT_TIMEOUT_S = 10.0


def reply_subject(agent_id: str) -> str:
    """A single-use reply subject inside the agent's own inbox namespace.

    The `srv` token separates these from the inboxes the agent creates for its
    own requests, so the two never collide.
    """
    return f"{agent_inbox_prefix(agent_id)}.srv.{secrets.token_hex(12)}"


async def request_agent(
    nc: nats.NATS,
    agent_id: str,
    op: str,
    payload: bytes,
    *,
    timeout: float = DEFAULT_TIMEOUT_S,  # noqa: ASYNC109 - nats sub.next_msg takes its own
) -> bytes:
    """Send `cmd.{agent_id}.{op}` and return the raw reply body.

    Raises nats.errors.TimeoutError if the agent does not answer in time. A
    timeout means only that: no answer. It does NOT mean the agent did not act
    on the request, and callers whose command has a side effect have to be
    written for that.
    """
    inbox = reply_subject(agent_id)
    # Subscribe BEFORE publishing. A fast agent on a local link can answer
    # before a subscription registered afterwards exists, and core NATS drops
    # a message with no subscriber rather than queueing it, so the reply is
    # simply gone and the caller waits out the full timeout.
    sub = await nc.subscribe(inbox)
    try:
        await nc.publish(cmd(agent_id, op), payload, reply=inbox)
        await nc.flush()
        msg = await sub.next_msg(timeout=timeout)
        return msg.data
    finally:
        await sub.unsubscribe()
