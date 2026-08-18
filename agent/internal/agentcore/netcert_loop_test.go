package agentcore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everwas/everwas/agent/internal/netcert"
)

// testAgentID is a UUIDv7, the shape of a real agent id, so the per-device
// renewal offset is exercised rather than sidestepped.
const testAgentID = "01a00b45-0e50-78c8-b572-8b8fbc272ad1"

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

// safeBuf is a log sink the test can read while the loop is still writing.
//
// Cancelling a context does not synchronously stop the goroutine it belongs
// to, so a plain bytes.Buffer here is a genuine data race between the loop's
// last log line and the assertion, which -race reports and which would
// otherwise show up as a rare, confusing CI failure.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor blocks until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// materialInPhase builds a real certificate positioned to land in the phase
// wanted.
//
// Built from actual NotBefore/NotAfter rather than a stubbed enum, so these
// tests exercise the same arithmetic that runs in production, including the
// per-device jitter. A test that hands the loop a phase directly never touches
// the code that decides the phase, which is where the interesting mistakes are.
func materialInPhase(phase netcert.Phase) *netcert.Material {
	const life = 100 * 24 * time.Hour
	now := time.Now()

	if phase == netcert.PhaseExpired {
		return &netcert.Material{
			NotBefore: now.Add(-2 * life), NotAfter: now.Add(-life), Serial: "expired-serial",
		}
	}
	var fraction float64
	switch phase {
	case netcert.PhaseFresh:
		fraction = 0.10
	case netcert.PhaseDue:
		// Clear of half life plus the largest jitter offset any device can get.
		fraction = 0.70
	case netcert.PhaseUrgent:
		fraction = 0.95
	}
	elapsed := time.Duration(fraction * float64(life))
	return &netcert.Material{
		NotBefore: now.Add(-elapsed),
		NotAfter:  now.Add(life - elapsed),
		Serial:    "abc123",
	}
}

// ensuring builds an ensure func with a fixed outcome, counting its calls.
func ensuring(calls *atomic.Int32, phase netcert.Phase, err error) func(context.Context) (*netcert.Material, error) {
	m := materialInPhase(phase)
	return func(context.Context) (*netcert.Material, error) {
		calls.Add(1)
		return m, err
	}
}

// loopCfg is the common wiring; each test overrides what it cares about.
func loopCfg(
	ensure func(context.Context) (*netcert.Material, error),
	every, urgent time.Duration,
	log *slog.Logger,
) netcertConfig {
	return netcertConfig{
		ensure:  ensure,
		agentID: testAgentID,
		every:   every,
		urgent:  urgent,
		log:     log,
	}
}

func TestTheMaterialsPhaseIsDerivedNotDeclared(t *testing.T) {
	// Guards the helper the rest of this file leans on: if materialInPhase
	// stopped producing what it claims, every escalation test below would go
	// quietly green while exercising the wrong phase.
	for _, want := range []netcert.Phase{
		netcert.PhaseFresh, netcert.PhaseDue, netcert.PhaseUrgent, netcert.PhaseExpired,
	} {
		if got := materialInPhase(want).PhaseAt(time.Now(), testAgentID); got != want {
			t.Errorf("materialInPhase(%v) actually lands in %v", want, got)
		}
	}
}

func TestTheCertificateIsRequestedImmediatelyNotOnTheFirstTick(t *testing.T) {
	// The tick is twelve hours away in production. A device that enrolled with
	// no certificate and waited for it would sit off the network for half a day
	// on every fresh install, which is exactly the bootstrap case 802.1X makes
	// painful to recover from.
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Intervals that cannot fire during this test.
		_ = netcertLoop(ctx, loopCfg(ensuring(&calls, netcert.PhaseFresh, nil),
			time.Hour, time.Hour, discard()))
	}()

	waitFor(t, "the startup request", func() bool { return calls.Load() >= 1 })
}

func TestAFailureKeepsTheLoopAlive(t *testing.T) {
	// The server being unreachable is the ordinary case, and it is harmless:
	// whatever the device already holds is untouched. Giving up would turn a
	// transient outage into a certificate that silently expires weeks later.
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- netcertLoop(ctx, loopCfg(
			ensuring(&calls, netcert.PhaseDue, errors.New("dial tcp: connection refused")),
			time.Millisecond, time.Millisecond, discard()))
	}()

	waitFor(t, "repeated attempts after a failure", func() bool { return calls.Load() >= 5 })
	select {
	case err := <-done:
		t.Fatalf("the loop exited on a transient failure: %v", err)
	default:
	}
}

func TestAnUnconfiguredServerIsReportedOnceNotOnEveryCycle(t *testing.T) {
	// Most deployments will never enable the CA. An agent that warns about an
	// unused feature on every cycle trains its operators to skim past its
	// warnings, which costs exactly when one of them finally matters.
	var buf safeBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = netcertLoop(ctx, loopCfg(
			ensuring(&calls, netcert.PhaseExpired, netcert.ErrNotConfigured),
			time.Millisecond, time.Millisecond, log))
	}()

	waitFor(t, "many cycles", func() bool { return calls.Load() >= 20 })
	cancel()

	if got := strings.Count(buf.String(), "not issuing device certificates"); got != 1 {
		t.Errorf("reported %d times across %d cycles, want exactly 1", got, calls.Load())
	}
}

func TestAnUnconfiguredServerNeverEscalates(t *testing.T) {
	// A server with no CA leaves the device holding nothing, which reads as
	// expired. That must NOT be treated as an emergency: there is no deadline
	// to miss, and popping a dialog at the user about a feature nobody enabled
	// would be the worst possible use of that channel.
	var buf safeBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))

	var calls, warned atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := loopCfg(ensuring(&calls, netcert.PhaseExpired, netcert.ErrNotConfigured),
		time.Millisecond, time.Millisecond, log)
	cfg.warnUser = func(context.Context, netcert.Phase, time.Time) { warned.Add(1) }
	go func() { _ = netcertLoop(ctx, cfg) }()

	waitFor(t, "several cycles", func() bool { return calls.Load() >= 10 })
	cancel()

	if warned.Load() != 0 {
		t.Errorf("interrupted the user %d times about a CA the server does not have", warned.Load())
	}
	if strings.Contains(buf.String(), "about to expire") {
		t.Error("raised an expiry alarm on a server that issues no certificates")
	}
}

func TestARoutineFailureDoesNotInterruptTheUser(t *testing.T) {
	// Weeks of margin left. Interrupting somebody's work over a failure that
	// will very likely resolve itself before it matters is how the warning
	// becomes noise, and this channel only works while it is still believed.
	var calls, warned atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := loopCfg(ensuring(&calls, netcert.PhaseDue, errors.New("server down")),
		time.Millisecond, time.Millisecond, discard())
	cfg.warnUser = func(context.Context, netcert.Phase, time.Time) { warned.Add(1) }
	go func() { _ = netcertLoop(ctx, cfg) }()

	waitFor(t, "several cycles", func() bool { return calls.Load() >= 10 })
	cancel()

	if warned.Load() != 0 {
		t.Errorf("interrupted the user %d times while there was still weeks of margin", warned.Load())
	}
}

func TestAnUrgentFailureInterruptsTheUserAndLogsLoudly(t *testing.T) {
	// The case the whole escalation exists for. Renewal has been failing for
	// most of the margin, the server cannot be told because the path to it is
	// what is broken, and the person at the machine is the only party left who
	// can do anything about it.
	var buf safeBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))

	var calls, warned atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := loopCfg(ensuring(&calls, netcert.PhaseUrgent, errors.New("server down")),
		time.Millisecond, time.Millisecond, log)
	cfg.warnUser = func(context.Context, netcert.Phase, time.Time) { warned.Add(1) }
	go func() { _ = netcertLoop(ctx, cfg) }()

	waitFor(t, "the user to be warned", func() bool { return warned.Load() >= 1 })
	cancel()

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Error("an imminent expiry was not logged at ERROR, so it reads like a routine retry")
	}
	if !strings.Contains(out, "about to expire") {
		t.Error("the log does not say what is actually wrong")
	}
}

func TestUrgencyShortensTheRetryInterval(t *testing.T) {
	// DHCP's T2: passing it changes the client's strategy rather than merely
	// repeating it. Retrying every twelve hours an hour before expiry throws
	// away nearly every remaining chance to recover.
	var urgent, routine atomic.Int32

	run := func(phase netcert.Phase, calls *atomic.Int32) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			// A long routine interval and a short urgent one: only a loop that
			// actually escalates will get past its first attempt.
			_ = netcertLoop(ctx, loopCfg(ensuring(calls, phase, errors.New("server down")),
				time.Hour, time.Millisecond, discard()))
		}()
		time.Sleep(150 * time.Millisecond)
	}

	run(netcert.PhaseUrgent, &urgent)
	run(netcert.PhaseDue, &routine)

	if routine.Load() != 1 {
		t.Errorf("routine phase made %d attempts, want 1: it should have waited the long interval",
			routine.Load())
	}
	if urgent.Load() < 5 {
		t.Errorf("urgent phase made only %d attempts, so it never escalated its cadence",
			urgent.Load())
	}
}

func TestWhatTheDeviceHoldsIsPublishedEvenWhenRenewalFails(t *testing.T) {
	// The whole point of reporting the serial: a failed renewal is EXACTLY
	// when what the device holds stops matching what the server last issued.
	// Publishing only on success would go quiet in the one case worth seeing.
	var calls atomic.Int32
	var got atomic.Value

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := loopCfg(ensuring(&calls, netcert.PhaseDue, errors.New("server down")),
		time.Millisecond, time.Millisecond, discard())
	cfg.observe = func(m *netcert.Material) {
		if m != nil {
			got.Store(m.Serial)
		}
	}
	go func() { _ = netcertLoop(ctx, cfg) }()

	waitFor(t, "the held certificate to be published", func() bool { return got.Load() != nil })
	cancel()

	if s, _ := got.Load().(string); s != "abc123" {
		t.Errorf("published serial = %q, want the one actually on disk", s)
	}
}

func TestCancellingStopsTheLoop(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- netcertLoop(ctx, loopCfg(ensuring(&calls, netcert.PhaseFresh, nil),
			time.Millisecond, time.Millisecond, discard()))
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the loop ignored cancellation, so shutdown would hang")
	}
}
