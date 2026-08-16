//go:build !windows

package scripts

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRunner returns a Runner with no NATS connection whose output is
// captured in memory.
func newTestRunner(t *testing.T) (*Runner, func() string) {
	t.Helper()
	r := NewRunner(nil, "agent-test", t.TempDir(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	var mu sync.Mutex
	var sb strings.Builder
	r.emit = func(c Chunk) error {
		mu.Lock()
		defer mu.Unlock()
		sb.Write(c.Data)
		return nil
	}
	return r, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

func TestRunnerStatuses(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	tests := []struct {
		name     string
		job      JobSpec
		want     string
		wantExit int
		contains string
	}{
		{
			name:     "success",
			job:      JobSpec{JobID: "j-ok", Shell: "bash", Body: "echo hello"},
			want:     StatusSucceeded,
			contains: "hello",
		},
		{
			name:     "nonzero exit is failed",
			job:      JobSpec{JobID: "j-fail", Shell: "bash", Body: "echo oops >&2; exit 3"},
			want:     StatusFailed,
			wantExit: 3,
			contains: "oops",
		},
		{
			name:     "empty body",
			job:      JobSpec{JobID: "j-empty", Shell: "bash"},
			want:     StatusFailed,
			wantExit: -1,
			contains: "empty script body",
		},
		{
			name:     "unknown shell",
			job:      JobSpec{JobID: "j-shell", Shell: "brainfuck", Body: "+++"},
			want:     StatusFailed,
			wantExit: -1,
			contains: "unsupported shell",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, output := newTestRunner(t)
			res := r.Run(context.Background(), tt.job, nil)
			if res.Status != tt.want || res.ExitCode != tt.wantExit {
				t.Errorf("got %s/%d, want %s/%d", res.Status, res.ExitCode, tt.want, tt.wantExit)
			}
			if !strings.Contains(output(), tt.contains) {
				t.Errorf("output %q does not contain %q", output(), tt.contains)
			}
		})
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	r, _ := newTestRunner(t)
	start := time.Now()
	res := r.Run(context.Background(), JobSpec{
		JobID:    "j-timeout",
		Shell:    "bash",
		Body:     "sleep 30 & sleep 30",
		TimeoutS: 1,
	}, nil)
	if res.Status != StatusTimeout {
		t.Errorf("status = %s, want %s", res.Status, StatusTimeout)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s; the background child kept the pipes open", elapsed)
	}
}

// TestRunnerCompletesWithASetsidDescendant is the regression for the drain
// deadlock. The old code waited for both output pipes to reach EOF before
// calling Wait, on the theory that the process-group SIGKILL always closes
// them. It does not: a descendant that calls setsid leaves the group, keeps
// the inherited write end open, and the job never completes. No timeout, no
// result, no cleanup of the staged script body, and the running map grows a
// permanent entry.
//
// The existing timeout test could not catch this because `sh -c "sleep 30"`
// is exec-replaced by the shell, so there is no descendant at all.
func TestRunnerCompletesWithASetsidDescendant(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("no setsid: %v", err)
	}
	r, output := newTestRunner(t)
	r.drainGrace = 500 * time.Millisecond

	done := make(chan Result, 1)
	go func() {
		done <- r.Run(context.Background(), JobSpec{
			JobID: "j-setsid",
			Shell: "bash",
			// The setsid child leaves the process group with stdout still
			// open. It outlives the kill by design; that is the point.
			Body:     "setsid sleep 30 & echo hello; sleep 30",
			TimeoutS: 1,
		}, nil)
	}()

	select {
	case res := <-done:
		if res.Status != StatusTimeout {
			t.Errorf("status = %s, want %s", res.Status, StatusTimeout)
		}
		if !strings.Contains(output(), "hello") {
			t.Errorf("output %q lost the data written before the kill", output())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the job never reported: a descendant outside the process group held the pipes open")
	}

	if running := r.Running(); len(running) != 0 {
		t.Errorf("running map still holds %v", running)
	}
	entries, err := os.ReadDir(filepath.Join(r.StateDir, "work"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the staged script body survived: %d entries left", len(entries))
	}
}

func TestRunnerCancel(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	r, _ := newTestRunner(t)
	done := make(chan Result, 1)
	go func() {
		done <- r.Run(context.Background(), JobSpec{
			JobID: "j-cancel", Shell: "bash", Body: "sleep 30", TimeoutS: 60,
		}, nil)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if r.Cancel("j-cancel") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("job never registered as running")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case res := <-done:
		if res.Status != StatusCancelled {
			t.Errorf("status = %s, want %s", res.Status, StatusCancelled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the job")
	}
	if r.Cancel("j-cancel") {
		t.Error("finished job should no longer be cancellable")
	}
}

func TestRunnerEnvironmentIsScrubbed(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	t.Setenv("OPENRMM_TEST_SECRET", "leaked")
	r, output := newTestRunner(t)
	res := r.Run(context.Background(), JobSpec{
		JobID: "j-env",
		Shell: "bash",
		Body:  `echo "secret=[${OPENRMM_TEST_SECRET}] given=[${GIVEN}] path=[${PATH:+set}]"`,
		Env:   map[string]string{"GIVEN": "yes"},
	}, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status = %s", res.Status)
	}
	got := output()
	if !strings.Contains(got, "secret=[]") {
		t.Errorf("host secret leaked into the job: %q", got)
	}
	if !strings.Contains(got, "given=[yes]") {
		t.Errorf("server-provided var missing: %q", got)
	}
	if !strings.Contains(got, "path=[set]") {
		t.Errorf("PATH should survive scrubbing: %q", got)
	}
}

func TestRunnerRemovesWorkdir(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	r, _ := newTestRunner(t)
	r.Run(context.Background(), JobSpec{
		JobID: "sched:nightly:1700000000", Shell: "bash", Body: "pwd",
	}, nil)

	work := filepath.Join(r.StateDir, "work")
	entries, err := os.ReadDir(work)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("work dir still holds %d entries", len(entries))
	}
}

func TestRunnerProgressPhases(t *testing.T) {
	if _, err := Resolve("bash"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	r, _ := newTestRunner(t)
	var phases []string
	var mu sync.Mutex
	r.Run(context.Background(), JobSpec{JobID: "j-p", Shell: "bash", Body: "true"},
		func(_ int, phase, _ string) {
			mu.Lock()
			defer mu.Unlock()
			phases = append(phases, phase)
		})
	want := []string{PhaseStarted, PhaseRunning, PhaseFinished}
	if strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Errorf("phases = %v, want %v", phases, want)
	}
}

func TestStageScriptPermissions(t *testing.T) {
	dir, script, err := stageScript(t.TempDir(), "sched:a:1", "echo hi", ".sh")
	if err != nil {
		t.Fatalf("stageScript: %v", err)
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
	sfi, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if perm := sfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("script mode = %o, want 700", perm)
	}
	if strings.ContainsAny(filepath.Base(dir), `:\/`) {
		t.Errorf("workdir name %q keeps characters illegal on Windows", filepath.Base(dir))
	}
}

func TestSafeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"01J9-abc", "01J9-abc"},
		{"sched:nightly:1700000000", "sched_nightly_1700000000"},
		{"../../etc/passwd", "_.._etc_passwd"}, // leading dots trimmed: no hidden dirs
		{"", "job"},
		{"...", "job"},
	}
	for _, tt := range tests {
		if got := safeName(tt.in); got != tt.want {
			t.Errorf("safeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
