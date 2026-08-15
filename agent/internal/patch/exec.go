package patch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
}

// runCmd executes a command with a scrubbed environment and returns its
// output and exit code. Context cancellation kills the process: every call
// site passes the job's context, so a cancelled patch job does not leave a
// package manager running.
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

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = scrubEnv(os.Environ(), noninteractiveEnv(opts.Env))
	// Package managers that find no terminal on stdin stop asking questions.
	cmd.Stdin = nil

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if opts.OnLine == nil {
		out, err := cmd.Output()
		return finish(ctx, string(out), stderr.String(), err)
	}

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return cmdResult{ExitCode: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return cmdResult{ExitCode: -1, Err: err}
	}
	var (
		stdout bytes.Buffer
		wg     sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		scan := bufio.NewScanner(pipe)
		scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scan.Scan() {
			line := scan.Text()
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			opts.OnLine(line)
		}
		// A scanner error (line too long, pipe closed by a kill) loses the
		// tail of the output but must not lose the exit status.
		if err := scan.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			stderr.WriteString("openrmm-agent: output read: " + err.Error() + "\n")
		}
	}()
	wg.Wait()
	return finish(ctx, stdout.String(), stderr.String(), cmd.Wait())
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
