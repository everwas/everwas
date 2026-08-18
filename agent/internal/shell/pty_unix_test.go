//go:build !windows

package shell

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestResolveShellUnix(t *testing.T) {
	tests := []struct {
		name    string
		shell   string
		wantErr bool
	}{
		{"auto", "auto", false},
		{"empty means auto", "", false},
		{"sh is always there", "sh", false},
		{"case insensitive", "SH", false},
		{"unknown name refused", "fish-but-not-installed", true},
		{"absolute paths are refused", "/bin/sh", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveShell(tt.shell)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveShell(%q): %v", tt.shell, err)
			}
			if !strings.HasPrefix(got, "/") {
				t.Errorf("resolveShell(%q) = %q, want an absolute path", tt.shell, got)
			}
		})
	}
}

// TestPTYRoundTrip drives a real pseudo-terminal: write a command, read the
// echoed output, resize, then confirm exit reporting.
func TestPTYRoundTrip(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	p, err := startPTY("sh", 80, 24)
	if err != nil {
		t.Fatalf("startPTY: %v", err)
	}
	defer func() { _ = p.Close() }()

	if _, err := p.Write([]byte("echo everwas-pty-ok\nexit 7\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := p.Resize(120, 40); err != nil {
		t.Errorf("resize: %v", err)
	}

	seen := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), "everwas-pty-ok") || err != nil {
				seen <- sb.String()
				return
			}
		}
	}()

	select {
	case out := <-seen:
		if !strings.Contains(out, "everwas-pty-ok") {
			t.Errorf("pty output %q missing the echoed marker", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for pty output")
	}

	code, err := p.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}
