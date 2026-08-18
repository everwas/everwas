package agentcore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/netcert"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

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

func TestTheCertificateIsRequestedImmediatelyNotOnTheFirstTick(t *testing.T) {
	// The tick is twelve hours away in production. A device that enrolled with
	// no certificate and waited for it would sit off the network for half a day
	// on every fresh install, which is exactly the bootstrap case 802.1X makes
	// painful to recover from.
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = netcertLoop(ctx, func(context.Context) error {
			calls.Add(1)
			return nil
		}, time.Hour, discard()) // a tick that cannot fire during this test
	}()

	waitFor(t, "the startup request", func() bool { return calls.Load() >= 1 })
}

func TestAFailureKeepsTheLoopAlive(t *testing.T) {
	// The server being unreachable is the ordinary case, and it is harmless:
	// whatever the device already holds is untouched. Giving up would turn a
	// transient outage into a certificate that silently expires weeks later,
	// locking the machine off the network with no remote way back.
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- netcertLoop(ctx, func(context.Context) error {
			calls.Add(1)
			return errors.New("dial tcp: connection refused")
		}, time.Millisecond, discard())
	}()

	// Several cycles, all failing.
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
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = netcertLoop(ctx, func(context.Context) error {
			calls.Add(1)
			return netcert.ErrNotConfigured
		}, time.Millisecond, log)
	}()

	// Many cycles, so a per-cycle log line would be unmistakable.
	waitFor(t, "many cycles", func() bool { return calls.Load() >= 20 })
	cancel()

	if got := strings.Count(buf.String(), "not issuing device certificates"); got != 1 {
		t.Errorf("reported %d times across %d cycles, want exactly 1",
			got, calls.Load())
	}
}

func TestAnUnconfiguredServerIsStillPolled(t *testing.T) {
	// Staying quiet must not mean giving up. An operator who enables the CA
	// after the fleet is deployed should not have to restart every agent, so
	// the loop keeps asking even once it has stopped saying so.
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- netcertLoop(ctx, func(context.Context) error {
			calls.Add(1)
			return netcert.ErrNotConfigured
		}, time.Millisecond, discard())
	}()

	waitFor(t, "polling to continue while quiet", func() bool { return calls.Load() >= 10 })
	select {
	case err := <-done:
		t.Fatalf("the loop exited because the server had no CA, so enabling one later "+
			"would need every agent restarted: %v", err)
	default:
	}
}

func TestCancellingStopsTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- netcertLoop(ctx, func(context.Context) error { return nil }, time.Millisecond, discard())
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
