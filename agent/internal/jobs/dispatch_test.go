package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everwas/everwas/agent/internal/inventory"
	"github.com/everwas/everwas/agent/internal/scripts"
)

func jobBody(t *testing.T, jobID, kind string) []byte {
	t.Helper()
	raw, err := json.Marshal(scripts.JobSpec{JobID: jobID, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// waitFor polls until cond holds, so a test never depends on a sleep being
// long enough on a loaded machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWaitConsumerReturnsWhenTheConsumerCloses is the regression for a job
// consumer that stopped permanently. nats.go calls the error handler once
// and then stops the subscription on a terminal pull error (a deleted
// consumer, which is what recreating the JOBS stream produces). consume()
// parked on ctx.Done(), so it never returned, the retry loop never rebound,
// and the agent went on heartbeating healthy while executing nothing.
func TestWaitConsumerReturnsWhenTheConsumerCloses(t *testing.T) {
	closed := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- waitConsumer(context.Background(), closed) }()

	close(closed)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waitConsumer returned nil; the retry loop would treat this as a clean stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitConsumer ignored the closed consumer and would never rebind")
	}
}

func TestWaitConsumerReturnsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitConsumer(ctx, make(chan struct{}))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitConsumer = %v, want the context error", err)
	}
}

// TestHandleJobDropsADuplicateDelivery is the regression for missing
// in-flight dedup. handleJob dispatches then acks, so an ack lost to a blip
// longer than ack_wait brings the job back and a SECOND execution starts for
// the same job id. Both use work/{job_id}/: one truncates the other's script
// and one removes the directory out from under the other. For patch.install
// it is two concurrent package manager runs.
func TestHandleJobDropsADuplicateDelivery(t *testing.T) {
	m := testModule(t)
	var running atomic.Int32
	var total atomic.Int32
	release := make(chan struct{})
	m.RefreshInventory = func(context.Context) error {
		running.Add(1)
		total.Add(1)
		<-release
		running.Add(-1)
		return nil
	}

	first := &fakeMsg{data: jobBody(t, "job-dup", scripts.KindInventoryRefresh)}
	m.handleJob(first)
	waitFor(t, "the first delivery to start", func() bool { return running.Load() == 1 })

	second := &fakeMsg{data: jobBody(t, "job-dup", scripts.KindInventoryRefresh)}
	m.handleJob(second)

	if !second.acked {
		t.Error("the duplicate was not acked, so JetStream will keep redelivering it")
	}
	if got := total.Load(); got != 1 {
		t.Fatalf("%d executions started for one job id, want 1", got)
	}
	close(release)
	waitFor(t, "the job to finish", func() bool { return len(m.RunningJobs()) == 0 })

	// A redelivery arriving AFTER completion must not run the job again.
	//
	// This previously asserted the opposite, that "the same id may legitimately
	// run again". It may not: job ids are server-assigned UUIDs that are never
	// reused, and JetStream redelivers only because it saw no ack. So a second
	// delivery of a finished id is always a retry of work that already
	// happened, never a new request.
	//
	// It is also the likelier of the two duplicate shapes. The in-flight
	// registry catches an overlapping redelivery; this one arrives while the
	// callback is blocked in reserve waiting for a slot, and lands after the
	// original has finished and left the registry.
	third := &fakeMsg{data: jobBody(t, "job-dup", scripts.KindInventoryRefresh)}
	m.handleJob(third)
	if !third.acked {
		t.Error("the post-completion redelivery was not acked, so JetStream keeps offering it " +
			"and every offer is another chance to run it twice")
	}
	if got := total.Load(); got != 1 {
		t.Fatalf("%d executions for one job id, want 1: a script ran twice, or a patch "+
			"install ran twice and reported failure for updates it had already applied", got)
	}
}

// TestDispatchIsBoundedByTheWorkerPool is the regression for unbounded
// `go m.execute(...)`. JOBS retains a week and the consumer reads from the
// start of the stream, so an agent that was off for three days used to
// reconnect and start every queued job at once.
func TestDispatchIsBoundedByTheWorkerPool(t *testing.T) {
	m := testModule(t)
	var running, peak atomic.Int32
	release := make(chan struct{})
	m.RefreshInventory = func(context.Context) error {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return nil
	}

	const jobs = maxConcurrentJobs + 3
	msgs := make([]*fakeMsg, jobs)
	for i := range msgs {
		msgs[i] = &fakeMsg{data: jobBody(t, "job-"+string(rune('a'+i)), scripts.KindInventoryRefresh)}
	}
	delivered := make(chan struct{})
	go func() {
		// The library delivers to this callback one message at a time, so a
		// handler that blocks on a full pool is what stops the server pushing
		// more work at us.
		for _, msg := range msgs {
			m.handleJob(msg)
		}
		close(delivered)
	}()

	waitFor(t, "the pool to fill", func() bool { return running.Load() >= maxConcurrentJobs })
	select {
	case <-delivered:
		t.Fatal("every job was accepted at once; the pool is not bounded")
	case <-time.After(200 * time.Millisecond):
	}
	if got := running.Load(); got > maxConcurrentJobs {
		t.Fatalf("%d jobs running at once, want at most %d", got, maxConcurrentJobs)
	}

	close(release)
	<-delivered
	waitFor(t, "the backlog to drain", func() bool { return len(m.RunningJobs()) == 0 })
	if got := peak.Load(); got > maxConcurrentJobs {
		t.Errorf("peak concurrency %d, want at most %d", got, maxConcurrentJobs)
	}
	for i, msg := range msgs {
		if !msg.acked {
			t.Errorf("job %d was never acked", i)
		}
	}
}

// TestCancelJobStopsAPatchInstall is the regression for a cancel API that
// lied. cmdJobCancel consulted only the script runner's registry, which
// PatchDeps.Execute never joins, so cancelling a four hour patch install
// answered {accepted:false, error:"job not running"} while it went on
// running as root.
func TestCancelJobStopsAPatchInstall(t *testing.T) {
	m := testModule(t)
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	m.Patch = PatchDeps{
		Log:    m.Log,
		Runner: m.Scripts,
		RefreshPatchState: func(ctx context.Context) (inventory.PatchState, error) {
			close(started)
			<-ctx.Done()
			cancelled <- ctx.Err()
			return inventory.PatchState{}, ctx.Err()
		},
	}

	msg := &fakeMsg{data: jobBody(t, "job-patch", scripts.KindPatchScan)}
	m.handleJob(msg)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the patch job never started")
	}

	got := m.cmdJobCancel([]byte(`{"job_id":"job-patch","requested_by":"ryan"}`))
	if !got.Accepted || got.Cancelled == nil || !*got.Cancelled {
		t.Fatalf("job.cancel reply = %+v, want it to accept a running patch job", got)
	}
	select {
	case err := <-cancelled:
		if err == nil {
			t.Error("the patch job's context was not cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the patch job kept running after a cancel the API said it accepted")
	}
}

// TestDrainJobsReportsWhatItCouldNotFinish is the regression for SIGTERM
// abandoning in-flight jobs. Shutdown used to drain NATS and exit while a
// script or a patch install was still running: no result was ever published,
// so the server showed the job running forever.
func TestDrainJobsReportsWhatItCouldNotFinish(t *testing.T) {
	logs, captured := capturingLogger()
	m := testModule(t)
	m.Log = logs
	m.shutdownGrace = 150 * time.Millisecond
	m.cancelGrace = 150 * time.Millisecond

	started := make(chan struct{})
	hold := make(chan struct{})
	sawCancel := make(chan bool, 1)
	m.RefreshInventory = func(ctx context.Context) error {
		close(started)
		// Deliberately ignores its context, the way a package manager
		// mid-transaction does.
		<-hold
		sawCancel <- ctx.Err() != nil
		return nil
	}

	m.handleJob(&fakeMsg{data: jobBody(t, "job-stuck", scripts.KindInventoryRefresh)})
	<-started

	done := make(chan struct{})
	go func() { m.drainJobs(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainJobs never returned; shutdown would hang instead of reporting")
	}

	if got := captured(); !strings.Contains(got, "job-stuck") ||
		!strings.Contains(got, "could not finish") {
		t.Errorf("shutdown said nothing about the abandoned job: %s", got)
	}
	close(hold)
	if !<-sawCancel {
		t.Error("the running job's context was never cancelled")
	}

	// Nothing new starts once we are stopping.
	late := &fakeMsg{data: jobBody(t, "job-late", scripts.KindInventoryRefresh)}
	m.handleJob(late)
	if late.acked {
		t.Error("a job was accepted during shutdown; it must be left on the stream")
	}
	if !late.naked {
		t.Error("a job refused during shutdown was not naked for redelivery")
	}
}

// TestRunJobSurvivesAPanic is the regression for a panic in job execution
// killing the whole agent. execute runs on its own goroutine, outside the
// supervisor's recover, and it is where server-supplied data gets parsed. A
// panic there took the process down, and because the panic can beat the ack
// the same job came back and crash-looped every agent that received it.
func TestRunJobSurvivesAPanic(t *testing.T) {
	m := testModule(t)
	m.RefreshInventory = func(context.Context) error {
		panic("inventory collector exploded")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runJob(context.Background(), scripts.JobSpec{
			JobID: "job-panic", Kind: scripts.KindInventoryRefresh,
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runJob never returned")
	}
}

// TestHandleJobSurvivesAPanickingJob proves the recover is on the dispatch
// path the consumer actually uses, not only on a helper.
func TestHandleJobSurvivesAPanickingJob(t *testing.T) {
	logs, captured := capturingLogger()
	m := testModule(t)
	m.Log = logs
	m.RefreshInventory = func(context.Context) error {
		panic("boom")
	}

	msg := &fakeMsg{data: jobBody(t, "job-panic", scripts.KindInventoryRefresh)}
	m.handleJob(msg)
	waitFor(t, "the panicking job to be cleaned up", func() bool {
		return len(m.RunningJobs()) == 0
	})
	if !msg.acked {
		t.Error("the job was not acked")
	}
	if got := captured(); !strings.Contains(got, "panic while running a job") {
		t.Errorf("the panic was not reported: %s", got)
	}
}

// capturingLogger returns a logger and a reader for everything written to it.
func capturingLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	var sb strings.Builder
	h := slog.NewTextHandler(&lockedWriter{mu: &mu, sb: &sb}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	return slog.New(h), func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	sb *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.Write(p)
}
