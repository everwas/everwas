package jobs

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/sched"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/shell"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// reply is the shape every command answers with. Replies are bare JSON, not
// envelopes: request/reply is core NATS, already correlated, and the server
// bridge reads these fields directly.
type reply struct {
	Accepted        bool              `json:"accepted"`
	Error           string            `json:"error,omitempty"`
	JobID           string            `json:"job_id,omitempty"`
	SessionID       string            `json:"session_id,omitempty"`
	ScheduleVersion int               `json:"schedule_version,omitempty"`
	Cancelled       *bool             `json:"cancelled,omitempty"`
	Rejected        []sched.Rejection `json:"rejected,omitempty"`
}

// handleCommand is a NATS callback, so it runs on the library's goroutine
// with no supervisor above it: a panic parsing a server-supplied payload
// would take the process down. Recover, answer the caller, keep serving.
func (m *Module) handleCommand(msg *nats.Msg) {
	defer func() {
		if r := recover(); r != nil {
			m.Log.Error("panic in command handler", "subject", msg.Subject,
				"panic", r, "stack", string(debug.Stack()))
			m.respond(msg, reply{Accepted: false,
				Error: fmt.Sprintf("agent internal error: %v", r)})
		}
	}()
	m.dispatchCommand(msg)
}

func (m *Module) dispatchCommand(msg *nats.Msg) {
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
	case "agent.update":
		m.respond(msg, m.cmdAgentUpdate(data))
	case "agent.rotate_creds":
		m.respond(msg, m.cmdRotateCreds(data))
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
	// The module's registry, not the script runner's: a patch install never
	// registered with the runner, so cancelling a four hour install that was
	// running as root answered "job not running" while it went on running.
	cancelled := m.CancelJob(req.JobID)
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
	version, rejected, err := m.Sched.Sync(doc)
	if err != nil {
		// The schedule is live in memory even if the disk write failed; say
		// so rather than pretending the sync did not happen.
		m.Log.Warn("schedule persist failed", "version", version, "err", err)
		return reply{Accepted: false, ScheduleVersion: version,
			Rejected: rejected, Error: err.Error()}
	}
	m.Audit.Emit(audit.ScheduleSynced, map[string]any{
		"schedule_version": version,
		"entries":          len(doc.Entries) - len(rejected),
		"rejected":         len(rejected),
	})
	if len(rejected) > 0 {
		// Not accepted: the rest of the schedule is in force, but the server
		// must not go on believing these entries will fire.
		m.Log.Warn("schedule entries rejected", "version", version,
			"rejected", rejected)
		return reply{Accepted: false, ScheduleVersion: version, Rejected: rejected,
			Error: rejectionSummary(rejected)}
	}
	m.Log.Info("schedule synced", "version", version, "entries", len(doc.Entries))
	return reply{Accepted: true, ScheduleVersion: version}
}

// cmdAgentUpdate accepts a self-update and runs it as a job. It is two-phase
// like every other long-running command: the reply says only that the work
// started, and the outcome arrives on agents.{id}.jobs.{job_id}.result.
//
// Everything that can refuse the update is checked BEFORE the reply, so a
// server that gets accepted=true knows a result is coming.
func (m *Module) cmdAgentUpdate(data []byte) reply {
	var req struct {
		JobID       string `json:"job_id"`
		RequestedBy string `json:"requested_by"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return reply{Accepted: false, Error: "bad agent.update payload: " + err.Error()}
	}
	// The job id is server-assigned and becomes a subject token. An agent
	// that invented its own would publish results the server is not reading.
	if err := wire.CheckIdentifier("job_id", req.JobID); err != nil {
		return reply{Accepted: false, Error: err.Error()}
	}
	if err := m.Update.ready(); err != nil {
		return reply{Accepted: false, JobID: req.JobID, Error: err.Error()}
	}

	jobCtx, release, err := m.reserve(scripts.JobSpec{JobID: req.JobID, Kind: scripts.KindAgentUpdate})
	if err != nil {
		return reply{Accepted: false, JobID: req.JobID, Error: err.Error()}
	}
	// The whole request rides in Body so the job handler decodes it once,
	// the same way patch ids do.
	spec := scripts.JobSpec{
		JobID:       req.JobID,
		Kind:        scripts.KindAgentUpdate,
		Body:        string(data),
		RequestedBy: req.RequestedBy,
	}
	go func() {
		defer release()
		m.runJob(jobCtx, spec)
	}()
	return reply{Accepted: true, JobID: req.JobID}
}

// cmdRotateCreds installs a new agent secret.
//
// The OLD secret keeps working server-side for a grace window, and that is
// what makes this safe to answer at all: if this reply is lost the server
// believes rotation failed while the agent is already on the new secret.
// With one valid secret at a time that combination is an unrecoverable
// lockout requiring a site visit. With an overlap the next reconnect
// succeeds on either one.
func (m *Module) cmdRotateCreds(data []byte) reply {
	var req struct {
		Secret      string `json:"agent_secret"`
		RequestedBy string `json:"requested_by"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return reply{Accepted: false, Error: "bad agent.rotate_creds payload: " + err.Error()}
	}
	if req.Secret == "" {
		return reply{Accepted: false, Error: "agent.rotate_creds carried no secret"}
	}
	if m.RotateSecret == nil {
		return reply{Accepted: false, Error: "credential rotation is not wired"}
	}
	if err := m.RotateSecret(req.Secret); err != nil {
		// Persisting failed, so the agent is still on the old secret. Say so
		// plainly: the server must NOT retire the old one.
		m.Log.Error("credential rotation failed", "err", err)
		return reply{Accepted: false, Error: "could not persist the new secret: " + err.Error()}
	}
	m.Audit.Emit(audit.CredentialsRotated, map[string]any{"requested_by": req.RequestedBy})
	m.Log.Info("agent secret rotated")
	return reply{Accepted: true}
}

// rejectionSummary names the entries in the error string too, for a server
// that only reads `error`.
func rejectionSummary(rejected []sched.Rejection) string {
	parts := make([]string, 0, len(rejected))
	for _, r := range rejected {
		parts = append(parts, r.EntryID+": "+r.Reason)
	}
	return fmt.Sprintf("%d schedule entries cannot be scheduled and were dropped: %s",
		len(rejected), strings.Join(parts, "; "))
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
