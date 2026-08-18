package shell

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/everwas/everwas/agent/internal/audit"
	"github.com/everwas/everwas/agent/internal/wire"
)

// Close reasons published in the ctl "closed" event.
const (
	reasonExit         = "exit"
	reasonIdle         = "idle"
	reasonDisconnected = "disconnected"
	reasonServerGone   = "server_gone"
	reasonRequested    = "requested"
	reasonShutdown     = "shutdown"
)

type session struct {
	id          string
	agentID     string
	shellName   string
	requestedBy string
	idleTimeout time.Duration

	nc  *nats.Conn
	log *slog.Logger
	aud *audit.Publisher

	pty  PTY
	flow *flowController
	buf  *ring

	// connected reports NATS connectivity; a field so the timeout logic can
	// be tested without a live connection.
	connected func() bool

	ctx     context.Context
	cancel  context.CancelFunc
	subs    []*nats.Subscription
	onClose func(string)

	startedAt time.Time

	// outMu serialises the output path: the read pump and the monitor's
	// reconnect flush both publish frames.
	outMu sync.Mutex

	mu             sync.Mutex
	lastInput      time.Time
	lastPing       time.Time
	sawPing        bool
	disconnectedAt time.Time
	bytesIn        int64
	bytesOut       int64
	closing        bool
}

// start spawns the PTY and wires the three server-facing subscriptions.
//
// The id check is deliberately repeated here even though OpenSession already
// made it: this function is what actually builds the subjects, and a future
// second caller must not be able to reintroduce the wildcard.
func (s *session) start(cols, rows uint16) error {
	if err := wire.CheckIdentifier("session_id", s.id); err != nil {
		return err
	}
	p, err := startPTY(s.shellName, cols, rows)
	if err != nil {
		return err
	}
	s.pty = p
	s.flow = newFlowController()
	s.buf = newRing(RingBytes)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.startedAt = time.Now()
	s.lastInput = s.startedAt
	if s.connected == nil {
		s.connected = s.nc.IsConnected
	}

	subs := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{wire.ShellIn(s.agentID, s.id), s.guard("in", s.onInput)},
		{wire.ShellResize(s.agentID, s.id), s.guard("resize", s.onResize)},
		{wire.ShellCtl(s.agentID, s.id), s.guard("ctl", s.onCtl)},
	}
	for _, sp := range subs {
		sub, err := s.nc.Subscribe(sp.subject, sp.handler)
		if err != nil {
			s.unsubscribe()
			s.cancel()
			s.closeAndReap(p)
			return err
		}
		s.subs = append(s.subs, sub)
	}

	go s.pumpOutput()
	go s.watchExit()
	go s.monitor()

	s.aud.Emit(audit.ShellOpened, map[string]any{
		"session_id":   s.id,
		"shell":        s.shellName,
		"requested_by": s.requestedBy,
		"cols":         cols,
		"rows":         rows,
	})
	s.log.Info("shell session opened", "session_id", s.id, "shell", s.shellName,
		"requested_by", s.requestedBy)
	return nil
}

// closeAndReap kills the child and waits for it. Close on its own is not
// enough on the setup path: it SIGKILLs the process group but nothing calls
// Wait, because watchExit (the only other caller) has not been started yet,
// so the child sits as a zombie for the life of the agent. Wait runs on its
// own goroutine so a PTY implementation that blocks cannot wedge the caller,
// which is holding the module lock.
func (s *session) closeAndReap(p PTY) {
	if err := p.Close(); err != nil {
		s.log.Debug("shell pty close", "session_id", s.id, "err", err)
	}
	go func() {
		if _, err := p.Wait(); err != nil {
			s.log.Debug("shell pty reap", "session_id", s.id, "err", err)
		}
	}()
}

// pumpOutput reads the PTY and publishes frames, blocking whenever flow
// control says the server is behind.
func (s *session) pumpOutput() {
	buf := make([]byte, MaxFrameBytes)
	for {
		if err := s.flow.wait(s.ctx); err != nil {
			return
		}
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.emit(buf[:n])
		}
		if err != nil {
			return // child exited or the master closed; watchExit reports it
		}
	}
}

// emit publishes one read's worth of output, or buffers it if NATS is down.
func (s *session) emit(data []byte) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if !s.connected() {
		s.markDisconnected()
		s.buf.write(data)
		return
	}
	s.flushLocked()
	s.publishFrames(data)
}

// flushLocked drains the disconnect buffer after the link comes back. A gap
// event goes first so the console can tell the user output was lost.
func (s *session) flushLocked() {
	s.mu.Lock()
	wasDown := !s.disconnectedAt.IsZero()
	s.disconnectedAt = time.Time{}
	s.mu.Unlock()
	if !wasDown {
		return
	}
	pending, dropped := s.buf.drain()
	if dropped {
		s.publishCtl(map[string]any{"event": "gap", "session_id": s.id})
	}
	s.publishFrames(pending)
	s.log.Info("shell session resumed", "session_id", s.id,
		"buffered_bytes", len(pending), "dropped", dropped)
}

// publishFrames splits data into ≤32 KiB raw frames. Callers hold outMu.
func (s *session) publishFrames(data []byte) {
	for len(data) > 0 {
		n := min(len(data), MaxFrameBytes)
		if err := s.nc.Publish(wire.ShellOut(s.agentID, s.id), data[:n]); err != nil {
			s.log.Warn("shell out publish", "session_id", s.id, "err", err)
			return
		}
		s.flow.sent(n)
		s.mu.Lock()
		s.bytesOut += int64(n)
		s.mu.Unlock()
		data = data[n:]
	}
}

// guard wraps a subscription callback. These run on the NATS library's
// goroutine with no supervisor above them, and they are where server-supplied
// frames get parsed, so a panic here would take the whole agent down and
// with it every other session and job. One bad frame kills the frame.
func (s *session) guard(name string, fn nats.MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("panic in shell handler", "session_id", s.id,
					"handler", name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn(msg)
	}
}

func (s *session) onInput(msg *nats.Msg) {
	if len(msg.Data) == 0 {
		return
	}
	s.mu.Lock()
	s.lastInput = time.Now()
	s.bytesIn += int64(len(msg.Data))
	s.mu.Unlock()
	if _, err := s.pty.Write(msg.Data); err != nil {
		s.log.Warn("shell input write", "session_id", s.id, "err", err)
	}
}

func (s *session) onResize(msg *nats.Msg) {
	var rs struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if err := json.Unmarshal(msg.Data, &rs); err != nil {
		s.log.Warn("shell resize decode", "session_id", s.id, "err", err)
		return
	}
	if err := s.pty.Resize(rs.Cols, rs.Rows); err != nil {
		s.log.Warn("shell resize", "session_id", s.id, "err", err)
	}
}

// onCtl handles the server side of the control channel: byte acks and
// liveness pings.
func (s *session) onCtl(msg *nats.Msg) {
	var ctl struct {
		Ack   *int64 `json:"ack"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(msg.Data, &ctl); err != nil {
		return // our own ctl publishes land here too; ignore anything odd
	}
	if ctl.Ack != nil {
		if resumed := s.flow.ack(*ctl.Ack); resumed {
			s.log.Debug("shell flow resumed", "session_id", s.id,
				"unacked", s.flow.unackedBytes())
		}
	}
	if ctl.Event == "ping" {
		s.mu.Lock()
		s.lastPing = time.Now()
		s.sawPing = true
		s.mu.Unlock()
	}
}

// watchExit turns child exit into a closed event carrying the exit code.
func (s *session) watchExit() {
	code, err := s.pty.Wait()
	if err != nil {
		s.log.Warn("shell wait", "session_id", s.id, "err", err)
	}
	s.close(reasonExit, code)
}

// monitor enforces the three timeouts: idle user, dead server, dead link.
func (s *session) monitor() {
	tick := time.NewTicker(monitorInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			if reason, ok := s.checkTimeouts(time.Now()); ok {
				s.close(reason, -1)
				return
			}
		}
	}
}

// checkTimeouts also drives the connection state machine: it notices the
// link coming back even when the PTY is silent.
func (s *session) checkTimeouts(now time.Time) (string, bool) {
	if s.connected() {
		s.outMu.Lock()
		s.flushLocked()
		s.outMu.Unlock()
	} else {
		s.markDisconnected()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.disconnectedAt.IsZero() && now.Sub(s.disconnectedAt) > disconnectGrace {
		return reasonDisconnected, true
	}
	if now.Sub(s.lastInput) > s.idleTimeout {
		return reasonIdle, true
	}
	// The watchdog is armed from session start, not by the first ping. Being
	// armed by the party it watches meant a bridge that died before its
	// first ping left a root PTY open with nobody attached until the idle
	// timeout, fifteen minutes later. A new session gets a longer window
	// because the console may still be attaching.
	since, deadline := s.lastPing, pingTimeout
	if !s.sawPing {
		since, deadline = s.startedAt, firstPingGrace
	}
	if now.Sub(since) > deadline {
		return reasonServerGone, true
	}
	return "", false
}

func (s *session) markDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disconnectedAt.IsZero() {
		s.disconnectedAt = time.Now()
	}
}

// close tears the session down exactly once.
func (s *session) close(reason string, code int) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	in, out := s.bytesIn, s.bytesOut
	s.mu.Unlock()

	s.publishCtl(map[string]any{
		"event":      "closed",
		"session_id": s.id,
		"reason":     reason,
		"code":       code,
	})
	s.unsubscribe()
	s.flow.release()
	s.cancel()
	if err := s.pty.Close(); err != nil {
		s.log.Debug("shell pty close", "session_id", s.id, "err", err)
	}

	dur := time.Since(s.startedAt)
	s.aud.Emit(audit.ShellClosed, map[string]any{
		"session_id":   s.id,
		"shell":        s.shellName,
		"requested_by": s.requestedBy,
		"reason":       reason,
		"exit_code":    code,
		"duration_s":   int64(dur.Seconds()),
		"bytes_in":     in,
		"bytes_out":    out,
	})
	s.log.Info("shell session closed", "session_id", s.id, "reason", reason,
		"code", code, "duration_s", int64(dur.Seconds()), "bytes_in", in, "bytes_out", out)
	if s.onClose != nil {
		s.onClose(s.id)
	}
}

func (s *session) unsubscribe() {
	for _, sub := range s.subs {
		if err := sub.Unsubscribe(); err != nil {
			s.log.Debug("shell unsubscribe", "session_id", s.id, "err", err)
		}
	}
	s.subs = nil
}

// publishCtl sends an agent-side control event. It is bare JSON, not an
// envelope: the ctl channel is symmetric with the server's {"ack": n}.
func (s *session) publishCtl(payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		s.log.Warn("shell ctl marshal", "session_id", s.id, "err", err)
		return
	}
	if err := s.nc.Publish(wire.ShellCtl(s.agentID, s.id), raw); err != nil {
		s.log.Warn("shell ctl publish", "session_id", s.id, "err", err)
	}
}
