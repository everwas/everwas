package shell

import (
	"io"
	"log/slog"
	"testing"
	"time"
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
