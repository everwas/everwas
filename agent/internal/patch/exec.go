package patch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Timeouts. A scan is allowed to be slow (mirrors, metadata refresh); an
// install is allowed to be very slow (a kernel plus a hundred packages on a
// small VM). Neither is unbounded: a hung package manager must eventually
// release the job slot.
const (
	scanTimeout    = 15 * time.Minute
	installTimeout = 2 * time.Hour
	quickTimeout   = 60 * time.Second

	// killGrace is how long a process group gets between SIGTERM and
	// SIGKILL.
	killGrace = 10 * time.Second

	// waitDelay bounds two kinds of unexpected delay in Wait: a child that
	// does not exit after being signalled, and a child that exits but leaves
	// its stdout pipe held open by a GRANDCHILD. The second one is what
	// wedged this function past its own timeout: apt-get is killed, the dpkg
	// it spawned inherits the pipe, and the drain goroutine waits forever on
	// an fd nobody is going to close.
	waitDelay = 20 * time.Second
)

// cmdResult is the outcome of one command. ExitCode is -1 when the process
// never ran or was killed before it could report one.
type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// ok reports a clean exit.
func (r cmdResult) ok() bool { return r.Err == nil && r.ExitCode == 0 }

// combined returns stdout and stderr joined, for error messages.
func (r cmdResult) combined() string {
	return strings.TrimSpace(strings.TrimSpace(r.Stdout) + "\n" + strings.TrimSpace(r.Stderr))
}

// tail returns the last n lines of the combined output, which is what an
// operator actually needs when a package manager fails.
func (r cmdResult) tail(n int) string {
	lines := strings.Split(strings.TrimRight(r.combined(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// execOptions configures one command run.
type execOptions struct {
	// Timeout bounds the run. Zero means quickTimeout.
	Timeout time.Duration
	// Env adds to (and overrides) the scrubbed host environment.
	Env map[string]string
	// OnLine, if set, is called for every complete stdout line as it
	// arrives, so long installs can report progress before they finish.
	OnLine func(string)

	// Scope runs the command in a transient systemd scope, outside the
	// agent's own cgroup, so restarting the agent does not take the package
	// manager with it. It is a no-op where systemd-run is unavailable.
	Scope bool

	// LetFinish leaves the child running when the deadline passes instead of
	// signalling it. A dpkg or rpm transaction that is killed halfway leaves
	// a package database only a human can repair, so for those the timeout
	// is reported and the transaction is allowed to complete. Everything
	// else, including the retry that follows, treats the run as failed.
	LetFinish bool
}

// runCmd executes a command with a scrubbed environment and returns its
// output and exit code. Context cancellation kills the whole process GROUP,
// not just the direct child, so a cancelled patch job does not leave a
// package manager holding a lock.
//
// A non-zero exit is NOT an error here. Package managers use exit codes as
// data (dnf check-update returns 100 when updates exist), so the caller
// decides what a code means.
func runCmd(ctx context.Context, opts execOptions, name string, args ...string) cmdResult {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = quickTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runName, runArgs := name, args
	if opts.Scope {
		runName, runArgs, _ = scopeCommand(name, args)
	}

	var cmd *exec.Cmd
	if opts.LetFinish {
		// No context wiring at all: cancellation must not signal this child.
		cmd = exec.Command(runName, runArgs...) //nolint:gosec // fixed binaries, validated args
	} else {
		cmd = exec.CommandContext(ctx, runName, runArgs...) //nolint:gosec // fixed binaries, validated args
		cmd.Cancel = func() error { return terminateGroup(cmd) }
		cmd.WaitDelay = waitDelay
	}
	setProcAttr(cmd)
	cmd.Env = scrubEnv(os.Environ(), noninteractiveEnv(opts.Env))
	// Package managers that find no terminal on stdin stop asking questions.
	cmd.Stdin = nil

	var stdout, stderr syncBuffer
	cmd.Stderr = &stderr

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return cmdResult{ExitCode: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return cmdResult{ExitCode: -1, Err: err}
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		scan := bufio.NewScanner(pipe)
		scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scan.Scan() {
			line := scan.Text()
			stdout.WriteString(line)
			stdout.WriteString("\n")
			if opts.OnLine != nil {
				opts.OnLine(line)
			}
		}
		// A scanner error (line too long, or the pipe closed underneath us
		// by the WaitDelay timer) loses the tail of the output but must not
		// lose the exit status.
		if err := scan.Err(); err != nil &&
			!errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, os.ErrClosed) {
			stderr.WriteString("openrmm-agent: output read: " + err.Error() + "\n")
		}
	}()

	done := make(chan error, 1)
	go func() {
		// Wait AFTER the pipe is drained: Wait closes it, and it also joins
		// the goroutine exec.Cmd uses to fill cmd.Stderr.
		<-drained
		done <- cmd.Wait()
	}()

	select {
	case waitErr := <-done:
		return finish(ctx, stdout.String(), stderr.String(), waitErr)
	case <-ctx.Done():
		if !opts.LetFinish {
			// The group has been signalled through Cancel, and WaitDelay
			// closes the pipes if a grandchild is still holding them, so
			// this wait is bounded whatever the child does.
			waitErr := <-done
			return finish(ctx, stdout.String(), stderr.String(), waitErr)
		}
		// Let the transaction finish. Half an install is a host somebody has
		// to repair by hand, so the job reports a timeout while the package
		// manager keeps its lock until it is done.
		go func() { <-done }()
		stderr.WriteString(fmt.Sprintf(
			"openrmm-agent: deadline reached, %s was left running so the transaction is not interrupted\n", name))
		return cmdResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: -1,
			Err:      ctx.Err(),
		}
	}
}

// syncBuffer is a bytes.Buffer that can be read while a goroutine is still
// writing to it. A LetFinish run returns before its child has exited, so the
// drain goroutine outlives the call.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) WriteString(str string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.WriteString(str)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// finish turns a wait error into a cmdResult, mapping a context deadline
// onto a clear error rather than the generic "signal: killed".
func finish(ctx context.Context, stdout, stderr string, err error) cmdResult {
	res := cmdResult{Stdout: stdout, Stderr: stderr}
	switch {
	case err == nil:
		return res
	case ctx.Err() != nil:
		res.ExitCode = -1
		res.Err = ctx.Err()
		return res
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res // a non-zero exit is data, not an error
		}
		res.ExitCode = -1
		res.Err = err
		return res
	}
}

// verifyContext derives the context for a "what actually landed" query. It
// deliberately survives the parent's cancellation with a short deadline of
// its own: asking dpkg or rpm what is installed is how the audit event stops
// guessing, and it matters most in the case where the install was cut short.
func verifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), quickTimeout)
}

// have reports whether a binary is on PATH.
func have(binary string) bool {
	_, err := exec.LookPath(binary)
	return err == nil
}

// fileExists is the reboot-flag test used by several backends.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pctOf turns a count into a 10..90 progress percentage, leaving room for
// the caller's own start and finish ticks.
func pctOf(seen, total int) int {
	if total <= 0 {
		return 10
	}
	if seen > total {
		seen = total
	}
	return 10 + (80 * seen / total)
}

// sleepCtx waits for d or until ctx is done, reporting whether the wait
// completed. Retry loops use it so a cancelled job stops backing off.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
