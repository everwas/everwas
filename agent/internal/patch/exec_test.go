package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grandchildScript writes a shell script that spawns a background child which
// inherits stdout, then waits. The background child is the whole point: the
// old test used `sh -c "sleep 30"`, which the shell exec-replaces, so there
// was no descendant and the bug could not appear.
func grandchildScript(t *testing.T, markerDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spawner.sh")
	body := `#!/bin/sh
# A package manager is a parent: apt-get runs dpkg, dnf runs rpm.
sh -c 'while :; do sleep 1; done' &
child=$!
echo "spawned $child"
printf '%s' "$child" > ` + filepath.Join(markerDir, "child.pid") + `
wait $child
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(nil) == nil
}

// TestRunCmdCancellationKillsTheWholeGroup is the H5 regression. Without
// Setpgid, group signalling and WaitDelay, cancelling this run does three bad
// things at once: the grandchild survives (holding the dpkg lock in real
// life), it keeps the inherited stdout pipe open so the drain never finishes,
// and runCmd therefore blocks past its own timeout while holding the install
// gate.
func TestRunCmdCancellationKillsTheWholeGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a unix concept; Windows patching runs in process through WUA")
	}
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	markerDir := t.TempDir()
	script := grandchildScript(t, markerDir)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := runCmd(ctx, execOptions{Timeout: 60 * time.Second}, "sh", script)
	elapsed := time.Since(start)

	if res.Err == nil {
		t.Fatal("a cancelled command must report an error")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", res.Err)
	}
	// The old code blocked here forever: the grandchild still held the write
	// end of the stdout pipe, so the scanner never saw EOF.
	if elapsed > 40*time.Second {
		t.Fatalf("runCmd took %s to return; the drain was waiting on a pipe the orphan holds", elapsed)
	}

	raw, err := os.ReadFile(filepath.Join(markerDir, "child.pid"))
	if err != nil {
		t.Skipf("the script never recorded a child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Skipf("unusable child pid %q", raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived cancellation; in production that is dpkg holding lock-frontend", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunCmdLetFinishLeavesTheTransactionAlone covers the dpkg policy: the
// job reports a timeout, but the package manager keeps running rather than
// being killed halfway through writing the package database.
func TestRunCmdLetFinishLeavesTheTransactionAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell test")
	}
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	dir := t.TempDir()
	done := filepath.Join(dir, "committed")
	script := filepath.Join(dir, "txn.sh")
	body := "#!/bin/sh\necho starting transaction\nsleep 1\nprintf 'committed' > " + done + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	start := time.Now()
	res := runCmd(context.Background(), execOptions{
		Timeout:   200 * time.Millisecond,
		LetFinish: true,
	}, "sh", script)

	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline reported", res.Err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("runCmd waited %s; a LetFinish run must return at the deadline", time.Since(start))
	}
	if !strings.Contains(res.Stderr, "left running") {
		t.Errorf("stderr = %q, want it to say the command was left running", res.Stderr)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(done); err == nil && string(body) == "committed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the transaction was killed at the deadline instead of being allowed to commit")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunCmdLetFinishOutputIsRaceFree exercises the case the race detector
// cares about: the drain goroutine is still writing when the caller reads.
func TestRunCmdLetFinishOutputIsRaceFree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shell test")
	}
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	res := runCmd(context.Background(), execOptions{
		Timeout:   150 * time.Millisecond,
		LetFinish: true,
		OnLine:    func(string) {},
	}, "sh", "-c", "i=0; while [ $i -lt 200 ]; do echo line $i; i=$((i+1)); sleep 0.01; done")
	if res.Err == nil {
		t.Fatal("expected the deadline to be reported")
	}
	_ = res.combined()
	time.Sleep(200 * time.Millisecond)
	_ = res.combined()
}
