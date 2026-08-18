package conn

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/everwas/everwas/agent/internal/config"
)

// TestConnectReportsAClosedConnection proves the agent is told when it has
// gone deaf. Connect retries forever, so it returns a usable handle even with
// nothing listening; closing that handle is the same terminal state the
// server produces with an unrecognised -ERR, and the callback is the only
// thing standing between that and a process that keeps heartbeating into a
// dead socket. Port 1 is never bound, so this touches no running stack.
func TestConnectReportsAClosedConnection(t *testing.T) {
	closed := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	nc, err := Connect(&config.Config{
		NATSURL:     "nats://127.0.0.1:1",
		AgentID:     "01991111-2222-7333-8444-555566667777",
		AgentSecret: "secret",
	}, log, func() { close(closed) }, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer nc.Close()

	nc.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection closed but onClosed never fired; a deaf agent would stay up forever")
	}
}

// TestConnectToleratesNoCallback keeps the callback optional, so a caller
// that genuinely does not care (a one-shot tool) does not panic.
func TestConnectToleratesNoCallback(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	nc, err := Connect(&config.Config{
		NATSURL:     "nats://127.0.0.1:1",
		AgentID:     "a",
		AgentSecret: "s",
	}, log, nil, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	nc.Close()
	// Give the closed handler a moment to run on its own goroutine.
	time.Sleep(100 * time.Millisecond)
}
