package jobs

import (
	"encoding/json"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/audit"
	"github.com/openrmm/agent/internal/sched"
	"github.com/openrmm/agent/internal/shell"
)

// reply is the shape every command answers with. Replies are bare JSON, not
// envelopes: request/reply is core NATS, already correlated, and the server
// bridge reads these fields directly.
type reply struct {
	Accepted        bool   `json:"accepted"`
	Error           string `json:"error,omitempty"`
	JobID           string `json:"job_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	ScheduleVersion int    `json:"schedule_version,omitempty"`
	Cancelled       *bool  `json:"cancelled,omitempty"`
}

func (m *Module) handleCommand(msg *nats.Msg) {
	op := strings.TrimPrefix(msg.Subject, "cmd."+m.AgentID+".")
	data := commandData(msg.Data)

	switch op {
	case "ping":
		m.respondRaw(msg, map[string]any{
			"pong":     true,
			"version":  m.Version,
			"agent_id": m.AgentID,
			"sessions": m.Shell.Count(),
		})
	case "shell.open":
		m.respond(msg, m.cmdShellOpen(data))
	case "shell.close":
		m.respond(msg, m.cmdShellClose(data))
	case "job.cancel":
		m.respond(msg, m.cmdJobCancel(data))
	case "sched.sync":
		m.respond(msg, m.cmdSchedSync(data))
	case "agent.update", "agent.rotate_creds":
		m.Audit.Emit(audit.CommandUnsupported, map[string]any{"command": op})
		m.respond(msg, reply{Accepted: false, Error: "unsupported: " + op + " lands in M4"})
	default:
		m.respond(msg, reply{Accepted: false, Error: "unknown command " + op})
	}
}

func (m *Module) cmdShellOpen(data []byte) reply {
	var spec shell.OpenSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return reply{Accepted: false, Error: "bad shell.open payload: " + err.Error()}
	}
	if err := m.Shell.OpenSession(spec); err != nil {
		m.Log.Warn("shell open refused", "session_id", spec.SessionID, "err", err)
		return reply{Accepted: false, SessionID: spec.SessionID, Error: err.Error()}
	}
	return reply{Accepted: true, SessionID: spec.SessionID}
}

func (m *Module) cmdShellClose(data []byte) reply {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return reply{Accepted: false, Error: "bad shell.close payload: " + err.Error()}
	}
	if err := m.Shell.Close(req.SessionID); err != nil {
		return reply{Accepted: false, SessionID: req.SessionID, Error: err.Error()}
	}
	return reply{Accepted: true, SessionID: req.SessionID}
}

func (m *Module) cmdJobCancel(data []byte) reply {
	var req struct {
		JobID       string `json:"job_id"`
		RequestedBy string `json:"requested_by"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return reply{Accepted: false, Error: "bad job.cancel payload: " + err.Error()}
	}
	cancelled := m.Scripts.Cancel(req.JobID)
	if !cancelled {
		return reply{Accepted: false, JobID: req.JobID, Cancelled: &cancelled,
			Error: "job not running"}
	}
	m.Audit.Emit(audit.JobCancelled, map[string]any{
		"job_id":       req.JobID,
		"requested_by": req.RequestedBy,
	})
	return reply{Accepted: true, JobID: req.JobID, Cancelled: &cancelled}
}

func (m *Module) cmdSchedSync(data []byte) reply {
	var doc sched.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return reply{Accepted: false, Error: "bad sched.sync payload: " + err.Error()}
	}
	version, err := m.Sched.Sync(doc)
	if err != nil {
		// The schedule is live in memory even if the disk write failed; say
		// so rather than pretending the sync did not happen.
		m.Log.Warn("schedule persist failed", "version", version, "err", err)
		return reply{Accepted: false, ScheduleVersion: version, Error: err.Error()}
	}
	m.Audit.Emit(audit.ScheduleSynced, map[string]any{
		"schedule_version": version,
		"entries":          len(doc.Entries),
	})
	m.Log.Info("schedule synced", "version", version, "entries", len(doc.Entries))
	return reply{Accepted: true, ScheduleVersion: version}
}

func (m *Module) respond(msg *nats.Msg, r reply) {
	m.respondRaw(msg, r)
}

func (m *Module) respondRaw(msg *nats.Msg, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		m.Log.Warn("command reply marshal", "subject", msg.Subject, "err", err)
		return
	}
	if msg.Reply == "" {
		return // fire-and-forget command; nothing to answer
	}
	if err := msg.Respond(raw); err != nil {
		m.Log.Warn("command reply", "subject", msg.Subject, "err", err)
	}
}
