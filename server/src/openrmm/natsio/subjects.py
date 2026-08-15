"""Single source of truth for NATS subjects on the server side.

Mirrors agent/internal/wire/subjects.go. Both are written against
docs/nats-subjects.md — change that file first.
"""

PROTOCOL_VERSION = 1

# --- agent -> server ---


def heartbeat(agent_id: str) -> str:
    return f"agents.{agent_id}.heartbeat"


def telemetry(agent_id: str) -> str:
    return f"agents.{agent_id}.telemetry"


def inventory(agent_id: str, kind: str) -> str:
    return f"agents.{agent_id}.inventory.{kind}"


def job_progress(agent_id: str, job_id: str) -> str:
    return f"agents.{agent_id}.jobs.{job_id}.progress"


def job_output(agent_id: str, job_id: str) -> str:
    return f"agents.{agent_id}.jobs.{job_id}.output"


def job_result(agent_id: str, job_id: str) -> str:
    return f"agents.{agent_id}.jobs.{job_id}.result"


def events(agent_id: str) -> str:
    return f"agents.{agent_id}.events"


def shell_out(agent_id: str, session_id: str) -> str:
    return f"agents.{agent_id}.shell.{session_id}.out"


def shell_ctl(agent_id: str, session_id: str) -> str:
    return f"agents.{agent_id}.shell.{session_id}.ctl"


# --- server -> agent ---


def jobs_queue(agent_id: str) -> str:
    return f"jobs.{agent_id}"


def cmd(agent_id: str, op: str) -> str:
    return f"cmd.{agent_id}.{op}"


def shell_in(agent_id: str, session_id: str) -> str:
    return f"agents.{agent_id}.shell.{session_id}.in"


def shell_resize(agent_id: str, session_id: str) -> str:
    return f"agents.{agent_id}.shell.{session_id}.rsz"


# --- auth-callout permission sets ---


def agent_durable(agent_id: str) -> str:
    """Durable consumer name the agent binds on the JOBS stream."""
    return f"agent-{agent_id}"


def agent_permissions(agent_id: str) -> dict[str, list[str]]:
    """Subject permissions pinned into the user JWT issued by the auth callout.

    Beyond its own namespace an agent needs a keyhole into the JetStream API:
    enough to bind and pull from ITS OWN durable consumer on JOBS, and to ack.
    Every JS subject below is scoped to this agent's durable name — an agent
    cannot touch another agent's consumer, nor any other stream.
    """
    durable = agent_durable(agent_id)
    return {
        "publish": [
            f"agents.{agent_id}.>",
            "_INBOX.>",
            "$JS.API.INFO",
            f"$JS.API.CONSUMER.INFO.JOBS.{durable}",
            f"$JS.API.CONSUMER.CREATE.JOBS.{durable}",
            f"$JS.API.CONSUMER.CREATE.JOBS.{durable}.>",
            f"$JS.API.CONSUMER.MSG.NEXT.JOBS.{durable}",
            "$JS.ACK.>",
        ],
        "subscribe": [
            f"cmd.{agent_id}.>",
            f"jobs.{agent_id}",
            f"agents.{agent_id}.shell.*.in",
            f"agents.{agent_id}.shell.*.rsz",
            f"agents.{agent_id}.shell.*.ctl",
            "_INBOX.>",
        ],
    }
