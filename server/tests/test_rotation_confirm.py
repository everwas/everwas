"""Confirming a rotation must work through the session shape production uses.

A review reported that verify_agent_secret's db.commit() crashed the NATS auth
callout, because its only caller wraps it in session_scope() (`sessionmaker() as
s, s.begin()`) and committing inside an explicit begin() block raises.

It does not, and I checked directly: SQLAlchemy objects to work emitted AFTER a
commit inside a begin() block, not to the commit itself, and that commit was the
last statement before the return. So the reported crash was not reachable.

The commit is gone anyway, because it was one added line away from being
reachable, and the consequence is severe: the exception would reach
auth_callout_responder's blanket except, msg.respond would never be called, and
the agent's connect would die on the callout timeout rather than on a decision,
with an operator-visible stack trace indistinguishable from a real server bug.

This test exists because every test in test_revocation.py calls
verify_agent_secret with a bare session and no begin(), a shape production never
uses, so none of them exercise this at all.

The sequence: an admin rotates a device's secret, and the agent later connects
presenting the NEW one while prev_secret_hash is still set. The clearing UPDATE
commits, verify_agent_secret returns True, and then the context manager exit
raises. The exception reaches auth_callout_responder's blanket except, so
msg.respond is NEVER called: the NATS server gets no authorization response and
the agent's connect dies on the callout timeout rather than on a decision.

It self-heals on the next reconnect, so it costs one failed connect per
rotation. What it also costs is diagnosability: the operator sees a stack trace
labelled "auth callout request failed", indistinguishable from a real server
bug, and rotation_in_flight reports the rotation confirmed by a connection that
was actually refused.

Every existing test in test_revocation.py calls verify_agent_secret with a bare
session and no begin(), which is a session shape production never uses, so none
of them could catch this.
"""

import pytest

from openrmm.db.engine import session_scope
from openrmm.models.device import Device, OsFamily
from openrmm.services.enrollment import rotate_agent_secret, verify_agent_secret
from openrmm.util.ids import uuid7

pytestmark = pytest.mark.usefixtures("pg_database")


async def test_confirming_a_rotation_through_session_scope_does_not_raise():
    """Exercised through session_scope(), the shape the callout actually uses."""
    from openrmm.models.device import AgentCredential
    from openrmm.services.enrollment import _sha256

    device_id = uuid7()
    async with session_scope() as db:
        db.add(Device(id=device_id, hostname="rot-host", os_family=OsFamily.linux, tags=[]))
        await db.flush()
        db.add(AgentCredential(device_id=device_id, secret_hash=_sha256("original-secret")))

    # Rotate, which leaves prev_secret_hash set for the grace window.
    async with session_scope() as db:
        new_secret = await rotate_agent_secret(db, device_id, actor="admin@example.com", force=True)

    # The agent connects on the NEW secret while the old one is still valid.
    # This is the branch that clears prev_* and used to commit mid-transaction.
    async with session_scope() as db:
        ok = await verify_agent_secret(db, device_id, new_secret)
    assert ok is True

    # And the clearing actually persisted, so rotation_in_flight is precise.
    async with session_scope() as db:
        cred = await db.get(AgentCredential, device_id)
        assert cred.prev_secret_hash is None
        assert cred.prev_valid_until is None
