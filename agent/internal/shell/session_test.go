package shell

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// newTestSession builds a session with no PTY and no NATS connection: only
// the timeout state machine is under test.
func newTestSession(connected bool) *session {
	return &session{
		id:          "sid-1",
		agentID:     "agent-1",
		idleTimeout: DefaultIdleTimeout,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		flow:        newFlowController(),
		buf:         newRing(RingBytes),
		connected:   func() bool { return connected },
	}
}

func TestCheckTimeouts(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		connected  bool
		setup      func(*session)
		wantReason string
		wantClose  bool
	}{
		{
			name: "healthy session stays open", connected: true,
			setup: func(s *session) {
				s.lastInput = now.Add(-time.Minute)
				s.lastPing, s.sawPing = now.Add(-10*time.Second), true
			},
		},
		{
			name: "idle past the timeout", connected: true,
			setup: func(s *session) {
				s.lastInput = now.Add(-DefaultIdleTimeout - time.Second)
			},
			wantReason: reasonIdle, wantClose: true,
		},
		{
			name: "idle exactly at the timeout is still alive", connected: true,
			setup: func(s *session) {
				s.lastInput = now.Add(-DefaultIdleTimeout)
			},
		},
		{
			name: "two missed pings tears down", connected: true,
			setup: func(s *session) {
				s.lastInput = now
				s.lastPing, s.sawPing = now.Add(-pingTimeout-time.Second), true
			},
			wantReason: reasonServerGone, wantClose: true,
		},
		{
			name: "one missed ping is tolerated", connected: true,
			setup: func(s *session) {
				s.lastInput = now
				s.lastPing, s.sawPing = now.Add(-35*time.Second), true
			},
		},
		{
			name: "a server that never pinged is not assumed dead", connected: true,
			setup: func(s *session) {
				s.lastInput = now
				s.lastPing, s.sawPing = time.Time{}, false
			},
		},
		{
			name: "disconnect inside the grace window keeps the pty", connected: false,
			setup: func(s *session) {
				s.lastInput = now
				s.disconnectedAt = now.Add(-30 * time.Second)
			},
		},
		{
			name: "disconnect past the grace window tears down", connected: false,
			setup: func(s *session) {
				s.lastInput = now
				s.disconnectedAt = now.Add(-disconnectGrace - time.Second)
			},
			wantReason: reasonDisconnected, wantClose: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSession(tt.connected)
			s.lastInput = now
			tt.setup(s)
			reason, closed := s.checkTimeouts(now)
			if closed != tt.wantClose || reason != tt.wantReason {
				t.Errorf("checkTimeouts = %q/%v, want %q/%v",
					reason, closed, tt.wantReason, tt.wantClose)
			}
		})
	}
}

// TestCheckTimeoutsClearsDisconnectOnReconnect proves the grace clock resets
// when the link returns, so a flapping connection never accumulates its way
// to a teardown.
func TestCheckTimeoutsClearsDisconnectOnReconnect(t *testing.T) {
	now := time.Now()
	s := newTestSession(true)
	s.lastInput = now
	s.disconnectedAt = now.Add(-30 * time.Second)

	if reason, closed := s.checkTimeouts(now); closed {
		t.Fatalf("closed with %q on a reconnected session", reason)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.disconnectedAt.IsZero() {
		t.Error("disconnect timestamp survived the reconnect")
	}
}

func TestIdleTimeout(t *testing.T) {
	tests := []struct {
		in   int
		want time.Duration
	}{
		{0, DefaultIdleTimeout},
		{-5, DefaultIdleTimeout},
		{60, time.Minute},
	}
	for _, tt := range tests {
		if got := idleTimeout(tt.in); got != tt.want {
			t.Errorf("idleTimeout(%d) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestPTYEnvForcesTerm(t *testing.T) {
	got := ptyEnv([]string{"PATH=/usr/bin", "TERM=dumb", "HOME=/root"})
	var terms int
	for _, kv := range got {
		if kv == "TERM=xterm-256color" {
			terms++
		}
		if kv == "TERM=dumb" {
			t.Error("inherited TERM=dumb survived")
		}
	}
	if terms != 1 {
		t.Errorf("TERM set %d times, want exactly 1: %v", terms, got)
	}
}

// TestOpenSessionRejectsUnsafeSessionID is the regression for the defect that
// made one message brick a machine. session_id is interpolated into three
// subscribe subjects; ">" made the agent's own subscribe illegal, the server
// answered -ERR 'Invalid Subject', and nats.go closed the connection for
// good. The process stayed up and deaf. "*" was quieter: it subscribed a root
// PTY to every other session's keystrokes.
//
// The assertion is on the error IDENTITY, not just on failure. Before the
// fix these ids also produced an error (from the nil connection, after the
// PTY had already been spawned), which is exactly the kind of accidental
// pass that hides a live defect.
func TestOpenSessionRejectsUnsafeSessionID(t *testing.T) {
	unsafe := []struct {
		name string
		id   string
	}{
		{"full wildcard closes the connection forever", ">"},
		{"token wildcard wiretaps every other session", "*"},
		{"wildcard inside a plausible id", "sid-1>"},
		{"parent directory", ".."},
		{"current directory", "."},
		{"embedded whitespace", "a b"},
		{"empty", ""},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tt := range unsafe {
		t.Run(tt.name, func(t *testing.T) {
			// No NATS connection and no audit publisher on purpose: reaching
			// either of them means the id was accepted far enough to matter.
			m := New(nil, "agent-1", nil, log)
			err := m.OpenSession(OpenSpec{SessionID: tt.id, Shell: "sh"})
			if err == nil {
				t.Fatalf("OpenSession(%q) was accepted", tt.id)
			}
			if !errors.Is(err, wire.ErrInvalidIdentifier) {
				t.Fatalf("OpenSession(%q) = %v, want it to wrap wire.ErrInvalidIdentifier", tt.id, err)
			}
			if n := m.Count(); n != 0 {
				t.Errorf("%d sessions live after a refusal, want 0", n)
			}
		})
	}
}

func TestOpenSessionAcceptsAUUID(t *testing.T) {
	// Only the validation gate is exercised: a good id then fails on the nil
	// connection, which proves it got past the check rather than through it.
	m := New(nil, "agent-1", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := m.OpenSession(OpenSpec{SessionID: "01991111-2222-7333-8444-555566667777", Shell: "nope"})
	if errors.Is(err, wire.ErrInvalidIdentifier) {
		t.Fatalf("a UUID session id was rejected as malformed: %v", err)
	}
}

// TestSessionStartRefusesUnsafeIDBeforeSpawning pins the second gate: start
// builds the subjects, so it validates too, and it does so before the PTY
// exists. A test that only covered OpenSession would let a future caller
// reintroduce the hole.
func TestSessionStartRefusesUnsafeIDBeforeSpawning(t *testing.T) {
	s := &session{
		id:  ">",
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := s.start(80, 24)
	if !errors.Is(err, wire.ErrInvalidIdentifier) {
		t.Fatalf("start = %v, want it to wrap wire.ErrInvalidIdentifier", err)
	}
	if s.pty != nil {
		t.Error("a PTY was spawned for a session that could never be wired up")
	}
}
