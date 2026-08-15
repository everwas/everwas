// Package audit publishes the agent's audit trail on agents.{id}.events.
// Every event is an envelope of type "event" whose data is
// {event, at, detail{}}; the EVENTS stream keeps them for 90 days.
package audit

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openrmm/agent/internal/wire"
)

// Event names. Kept as constants so a typo fails the build, not the audit log.
const (
	ShellOpened        = "shell.opened"
	ShellClosed        = "shell.closed"
	ScriptExecuted     = "script.executed"
	JobCancelled       = "job.cancelled"
	SchedMisfireSkip   = "sched.misfire_skipped"
	ScheduleSynced     = "schedule.synced"
	CommandUnsupported = "command.unsupported"
)

type record struct {
	Event  string         `json:"event"`
	At     time.Time      `json:"at"`
	Detail map[string]any `json:"detail"`
}

// Publisher emits audit events. A nil Publisher is a no-op, which keeps
// tests and offline code paths from needing a live connection.
type Publisher struct {
	nc      *nats.Conn
	agentID string
	log     *slog.Logger
}

// New returns a Publisher bound to a connection and agent identity.
func New(nc *nats.Conn, agentID string, log *slog.Logger) *Publisher {
	return &Publisher{nc: nc, agentID: agentID, log: log}
}

// Emit publishes one audit event. Failures are logged, never returned: an
// audit publish must not change the outcome of the operation it describes.
func (p *Publisher) Emit(event string, detail map[string]any) {
	if p == nil || p.nc == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	env, err := wire.NewEnvelope("event", p.agentID, wire.NewMsgID(), record{
		Event:  event,
		At:     time.Now().UTC(),
		Detail: detail,
	})
	if err != nil {
		p.logWarn("audit envelope", event, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		p.logWarn("audit marshal", event, err)
		return
	}
	err = p.nc.PublishMsg(&nats.Msg{
		Subject: wire.Events(p.agentID),
		Data:    raw,
		Header:  nats.Header{"Nats-Msg-Id": []string{env.MsgID}},
	})
	if err != nil {
		p.logWarn("audit publish", event, err)
	}
}

func (p *Publisher) logWarn(msg, event string, err error) {
	if p.log != nil {
		p.log.Warn(msg, "event", event, "err", err)
	}
}
