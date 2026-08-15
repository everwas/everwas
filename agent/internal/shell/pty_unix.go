//go:build !windows

package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/creack/pty"
)

type unixPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

// startPTY spawns shellName on a new pseudo-terminal.
func startPTY(shellName string, cols, rows uint16) (PTY, error) {
	path, err := resolveShell(shellName)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path)
	cmd.Env = ptyEnv(os.Environ())
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	f, err := pty.StartWithSize(cmd, winsize(cols, rows))
	if err != nil {
		return nil, fmt.Errorf("start pty for %s: %w", path, err)
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(p.f, winsize(cols, rows))
}

// Close hangs up the terminal and then kills the session's process group, so
// a shell that ignores SIGHUP still goes away.
func (p *unixPTY) Close() error {
	err := p.f.Close()
	if p.cmd.Process != nil {
		if kerr := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); kerr != nil {
			_ = p.cmd.Process.Kill()
		}
	}
	return err
}

func (p *unixPTY) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

func winsize(cols, rows uint16) *pty.Winsize {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return &pty.Winsize{Cols: cols, Rows: rows}
}

// resolveShell maps the requested shell name onto an executable. Only known
// names are accepted: the shell field comes off the wire, and an arbitrary
// path here would be a second, sloppier way to run code.
func resolveShell(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto", "default":
		if sh := os.Getenv("SHELL"); sh != "" && isExecutable(sh) {
			return sh, nil
		}
		for _, c := range []string{"/bin/bash", "/bin/sh"} {
			if isExecutable(c) {
				return c, nil
			}
		}
		return "", fmt.Errorf("no shell found (tried $SHELL, /bin/bash, /bin/sh)")
	case "bash", "zsh", "sh":
		path, err := exec.LookPath(strings.ToLower(strings.TrimSpace(name)))
		if err != nil {
			return "", fmt.Errorf("shell %q not found: %w", name, err)
		}
		return path, nil
	default:
		return "", fmt.Errorf("unsupported shell %q", name)
	}
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
