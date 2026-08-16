package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/update"
)

func updateSpec(t *testing.T, req update.Request) scripts.JobSpec {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return scripts.JobSpec{JobID: "job-1", Kind: scripts.KindAgentUpdate, Body: string(body)}
}

// deps builds an UpdateDeps whose Apply returns whatever the test wants and
// whose Restart records that it was asked for, without touching a filesystem
// or a NATS connection.
func deps(out *update.Result, err error) (UpdateDeps, *[]string) {
	restarts := []string{}
	d := UpdateDeps{
		StateDir: "/tmp/does-not-need-to-exist",
		Version:  "2026.8.1",
		Apply: func(context.Context, update.Request, update.Options) (*update.Result, error) {
			return out, err
		},
	}
	d.Restart = func(reason string) { restarts = append(restarts, reason) }
	return d, &restarts
}

// TestSuccessfulUpdateRestarts is the whole point of the command. A swap that
// is not followed by an exit leaves the old code running against a new binary
// on disk, and the server has been told the host is updated.
func TestSuccessfulUpdateRestarts(t *testing.T) {
	d, restarts := deps(&update.Result{Version: "2026.8.16", Status: update.StatusApplied}, nil)

	res := d.Execute(context.Background(), updateSpec(t, update.Request{Version: "2026.8.16"}), nil)

	if res.Status != scripts.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	if res.UpdatedTo != "2026.8.16" {
		t.Errorf("updated_to = %q; the server cannot confirm the version landed", res.UpdatedTo)
	}
	if len(*restarts) != 1 {
		t.Fatalf("restarts = %v, want exactly one: without it the agent keeps running the old "+
			"binary while reporting the update applied", *restarts)
	}
}

// TestFinalizingUpdateDoesNotRestart covers the Windows hand-off. The helper
// process does the swap after this one exits and restarts the service itself;
// racing it from here would kill the agent mid-swap.
func TestFinalizingUpdateDoesNotRestart(t *testing.T) {
	d, restarts := deps(&update.Result{
		Version: "2026.8.16", Finalizing: true, FinalizerPID: 4242,
	}, nil)

	res := d.Execute(context.Background(), updateSpec(t, update.Request{Version: "2026.8.16"}), nil)

	if !res.Finalizing {
		t.Error("finalizing was not reported, so the server records the host as updated while " +
			"it is still on the old version")
	}
	if len(*restarts) != 0 {
		t.Errorf("restarts = %v, want none while a finalizer is doing the swap", *restarts)
	}
}

// TestRefusalsDoNotRestart: every path that did not swap anything must leave
// the process alone. Restarting on a refusal turns a declined update into a
// service bounce, fleet-wide, on a loop.
func TestRefusalsDoNotRestart(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     string
		exitCode int
	}{
		{"already current", update.ErrAlreadyCurrent, scripts.StatusSucceeded, exitAlreadyCurrent},
		{"version denied", update.ErrVersionDenied, scripts.StatusFailed, exitVersionDenied},
		{"finalize pending", update.ErrFinalizePending, scripts.StatusFailed, exitFinalizePending},
		{"download failed", errors.New("connection reset"), scripts.StatusFailed, -1},
		{"cancelled", context.Canceled, scripts.StatusCancelled, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, restarts := deps(nil, tc.err)

			res := d.Execute(context.Background(),
				updateSpec(t, update.Request{Version: "2026.8.16"}), nil)

			if res.Status != tc.want {
				t.Errorf("status = %q, want %q", res.Status, tc.want)
			}
			if res.ExitCode != tc.exitCode {
				t.Errorf("exit_code = %d, want %d: the server cannot tell WHY without it",
					res.ExitCode, tc.exitCode)
			}
			if len(*restarts) != 0 {
				t.Errorf("restarts = %v, want none: nothing was swapped", *restarts)
			}
		})
	}
}

// TestAlreadyCurrentIsNotAFailure: a ring rollout re-sends the same version to
// hosts it is unsure about. Reporting "already running it" as a failure stalls
// the ring on exactly the hosts that are already done.
func TestAlreadyCurrentIsNotAFailure(t *testing.T) {
	d, _ := deps(nil, update.ErrAlreadyCurrent)

	res := d.Execute(context.Background(), updateSpec(t, update.Request{Version: "2026.8.1"}), nil)

	if res.Status != scripts.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
	if res.UpdatedTo != "2026.8.1" {
		t.Errorf("updated_to = %q, want the version actually running", res.UpdatedTo)
	}
}

// TestUnwiredRestartRefusesBeforeDownloading. An agent that cannot restart
// must not swap its binary: the file changes, the running code does not, and
// the next crash rolls back a version that was never actually tried.
func TestUnwiredRestartRefusesBeforeDownloading(t *testing.T) {
	applied := false
	d := UpdateDeps{
		StateDir: "/tmp/does-not-need-to-exist",
		Version:  "2026.8.1",
		Apply: func(context.Context, update.Request, update.Options) (*update.Result, error) {
			applied = true
			return &update.Result{Version: "2026.8.16"}, nil
		},
		// Restart deliberately nil.
	}

	res := d.Execute(context.Background(), updateSpec(t, update.Request{Version: "2026.8.16"}), nil)

	if res.Status != scripts.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if applied {
		t.Error("the binary was swapped by an agent that has no way to restart into it")
	}
}

// TestNilResultWithNilErrorIsNotSuccess. A pipeline that returns neither is a
// bug, but reporting success for it would tell the server a host updated when
// nothing was verified, and ring rollouts advance on that signal.
func TestNilResultWithNilErrorIsNotSuccess(t *testing.T) {
	d, restarts := deps(nil, nil)

	res := d.Execute(context.Background(), updateSpec(t, update.Request{Version: "2026.8.16"}), nil)

	if res.Status != scripts.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if len(*restarts) != 0 {
		t.Errorf("restarts = %v, want none", *restarts)
	}
}

// TestBadPayloadIsATerminalResult. A malformed request must still reach a
// terminal state: the alternative is a job the console shows as running for
// ever with no way to clear it.
func TestBadPayloadIsATerminalResult(t *testing.T) {
	d, _ := deps(&update.Result{Version: "x"}, nil)

	res := d.Execute(context.Background(), scripts.JobSpec{
		JobID: "job-1", Kind: scripts.KindAgentUpdate, Body: "{not json",
	}, nil)

	if res.Status != scripts.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
}
