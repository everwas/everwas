// Package shell runs interactive PTY sessions for the browser console.
// Output is raw bytes on agents.{id}.shell.{sid}.out with explicit
// byte-level flow control on .ctl, because core NATS drops messages for
// slow consumers instead of pushing back.
package shell

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

const (
	// MaxSessions caps concurrent PTYs on one host.
	MaxSessions = 5

	// DefaultIdleTimeout closes a session with no keyboard input.
	DefaultIdleTimeout = 15 * time.Minute

	// pingTimeout is two missed 30 s server pings plus slack.
	pingTimeout = 65 * time.Second

	// firstPingGrace is how long a new session waits for the bridge's first
	// ping. Generous, because the console is still attaching, but finite:
	// a bridge that dies before it ever pings must not leave a root PTY
	// running until the idle timeout.
	firstPingGrace = 90 * time.Second

	// disconnectGrace is how long a PTY survives a NATS outage.
	disconnectGrace = 60 * time.Second

	monitorInterval = 5 * time.Second
)

// Errors the command handler translates into {"accepted": false, "error"}.
var (
	ErrTooManySessions = errors.New("too many concurrent shell sessions")
	ErrSessionExists   = errors.New("shell session already open")
	ErrNoSession       = errors.New("no such shell session")
	ErrShuttingDown    = errors.New("agent is shutting down")
)

// Module owns the set of live sessions.
type Module struct {
	nc      *nats.Conn
	agentID string
	aud     *audit.Publisher
	log     *slog.Logger

	maxSessions int

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// New returns a Module. Call Run to tie its lifetime to a context.
func New(nc *nats.Conn, agentID string, aud *audit.Publisher, log *slog.Logger) *Module {
	return &Module{
		nc:          nc,
		agentID:     agentID,
		aud:         aud,
		log:         log,
		maxSessions: MaxSessions,
		sessions:    map[string]*session{},
	}
}

// OpenSpec is the decoded cmd.{id}.shell.open payload.
type OpenSpec struct {
	SessionID    string `json:"session_id"`
	Shell        string `json:"shell"`
	Cols         uint16 `json:"cols"`
	Rows         uint16 `json:"rows"`
	IdleTimeoutS int    `json:"idle_timeout_s"`
	RequestedBy  string `json:"requested_by"`
}

// Open starts a session with the default idle timeout.
func (m *Module) Open(sessionID, shellName string, cols, rows uint16, requestedBy string) error {
	return m.OpenSession(OpenSpec{
		SessionID:   sessionID,
		Shell:       shellName,
		Cols:        cols,
		Rows:        rows,
		RequestedBy: requestedBy,
	})
}

// OpenSession starts a session from a full spec.
//
// The session id is validated first, before anything is spawned. It is
// interpolated into three subscribe subjects, so a wildcard in it either
// wiretaps every other session or kills this agent's NATS connection for
// good. Refusing costs one reply; accepting costs the machine.
func (m *Module) OpenSession(spec OpenSpec) error {
	if err := wire.CheckIdentifier("session_id", spec.SessionID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrShuttingDown
	}
	if _, ok := m.sessions[spec.SessionID]; ok {
		return ErrSessionExists
	}
	if len(m.sessions) >= m.maxSessions {
		return fmt.Errorf("%w (limit %d)", ErrTooManySessions, m.maxSessions)
	}

	s := &session{
		id:          spec.SessionID,
		agentID:     m.agentID,
		shellName:   spec.Shell,
		requestedBy: spec.RequestedBy,
		idleTimeout: idleTimeout(spec.IdleTimeoutS),
		nc:          m.nc,
		log:         m.log.With("component", "shell"),
		aud:         m.aud,
		onClose:     m.forget,
	}
	if err := s.start(spec.Cols, spec.Rows); err != nil {
		return err
	}
	m.sessions[spec.SessionID] = s
	return nil
}

// Close tears down one session on server request.
func (m *Module) Close(sessionID string) error {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return ErrNoSession
	}
	s.close(reasonRequested, -1)
	return nil
}

// Count returns the number of live sessions.
func (m *Module) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Run blocks until ctx is cancelled, then closes every session. It is the
// supervisor task entry point.
func (m *Module) Run(ctx context.Context) error {
	<-ctx.Done()
	m.shutdown()
	return ctx.Err()
}

func (m *Module) shutdown() {
	m.mu.Lock()
	m.closed = true
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.close(reasonShutdown, -1)
	}
}

func (m *Module) forget(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

func idleTimeout(sec int) time.Duration {
	if sec <= 0 {
		return DefaultIdleTimeout
	}
	return time.Duration(sec) * time.Second
}
