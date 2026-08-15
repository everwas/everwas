//go:build windows

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/UserExistsError/conpty"
)

type windowsPTY struct {
	cpty *conpty.ConPty

	once sync.Once
	code int
	err  error
}

// startPTY spawns shellName on a ConPTY. ConPTY needs Windows 10 1809 or
// later; on anything older this fails cleanly instead of hanging.
func startPTY(shellName string, cols, rows uint16) (PTY, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, fmt.Errorf("ConPTY unavailable (needs Windows 10 1809 or later)")
	}
	cmdline, err := resolveShell(shellName)
	if err != nil {
		return nil, err
	}
	cp, err := conpty.Start(cmdline,
		conpty.ConPtyDimensions(int(sane(cols, 80)), int(sane(rows, 24))),
		conpty.ConPtyEnv(ptyEnv(os.Environ())),
	)
	if err != nil {
		return nil, fmt.Errorf("start conpty for %s: %w", cmdline, err)
	}
	return &windowsPTY{cpty: cp}, nil
}

func (p *windowsPTY) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *windowsPTY) Write(b []byte) (int, error) { return p.cpty.Write(b) }

func (p *windowsPTY) Resize(cols, rows uint16) error {
	return p.cpty.Resize(int(sane(cols, 80)), int(sane(rows, 24)))
}

func (p *windowsPTY) Close() error { return p.cpty.Close() }

// Wait is guarded by a Once because conpty.Wait consumes the process handle;
// the session may reach it from both the exit watcher and teardown.
func (p *windowsPTY) Wait() (int, error) {
	p.once.Do(func() {
		code, err := p.cpty.Wait(context.Background())
		if err != nil {
			p.code, p.err = -1, err
			return
		}
		p.code = int(code)
	})
	return p.code, p.err
}

func sane(v, fallback uint16) uint16 {
	if v == 0 {
		return fallback
	}
	return v
}

// resolveShell maps a requested shell onto a Windows command line. As on
// unix, only known names are accepted.
func resolveShell(name string) (string, error) {
	var candidates []string
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto", "default":
		candidates = []string{"powershell.exe", "pwsh.exe", "cmd.exe"}
	case "powershell":
		candidates = []string{"powershell.exe", "pwsh.exe"}
	case "pwsh":
		candidates = []string{"pwsh.exe", "powershell.exe"}
	case "cmd":
		candidates = []string{"cmd.exe"}
	case "bash", "sh":
		candidates = []string{"bash.exe"}
	default:
		return "", fmt.Errorf("unsupported shell %q", name)
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("shell %q not found (tried %s)", name, strings.Join(candidates, ", "))
}
