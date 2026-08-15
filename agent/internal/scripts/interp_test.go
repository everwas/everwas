package scripts

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// fakeLookPath resolves only the names in present.
func fakeLookPath(present ...string) lookupFunc {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestCanonicalShellAliases(t *testing.T) {
	tests := []struct{ in, want string }{
		{"bash", "bash"},
		{"BASH", "bash"},
		{"  zsh  ", "zsh"},
		{"python3", "python"},
		{"py", "python"},
		{"powershell.exe", "powershell"},
		{"pwsh.exe", "pwsh"},
		{"cmd.exe", "cmd"},
		{"bat", "cmd"},
		{"", defaultShell()},
		{"auto", defaultShell()},
		{"default", defaultShell()},
	}
	for _, tt := range tests {
		if got := canonicalShell(tt.in); got != tt.want {
			t.Errorf("canonicalShell(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveWith(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix interpreter table")
	}
	tests := []struct {
		name     string
		shell    string
		present  []string
		wantPath string
		wantArgs []string
		wantExt  string
		wantErr  bool
	}{
		{name: "bash present", shell: "bash", present: []string{"bash"},
			wantPath: "/usr/bin/bash", wantExt: ".sh"},
		{name: "bash falls back to sh", shell: "bash", present: []string{"sh"},
			wantPath: "/usr/bin/sh", wantExt: ".sh"},
		{name: "auto resolves to bash", shell: "auto", present: []string{"bash"},
			wantPath: "/usr/bin/bash", wantExt: ".sh"},
		{name: "sh does not fall back to bash", shell: "sh", present: []string{"bash"},
			wantErr: true},
		{name: "zsh", shell: "zsh", present: []string{"zsh"},
			wantPath: "/usr/bin/zsh", wantExt: ".sh"},
		{name: "python prefers python3", shell: "python", present: []string{"python3", "python"},
			wantPath: "/usr/bin/python3", wantExt: ".py"},
		{name: "python falls back", shell: "python", present: []string{"python"},
			wantPath: "/usr/bin/python", wantExt: ".py"},
		{name: "powershell on unix omits ExecutionPolicy", shell: "powershell", present: []string{"pwsh"},
			wantPath: "/usr/bin/pwsh", wantExt: ".ps1",
			wantArgs: []string{"-NoProfile", "-NonInteractive", "-File"}},
		{name: "unknown shell", shell: "fish", present: []string{"fish"}, wantErr: true},
		{name: "known shell missing from PATH", shell: "zsh", present: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWith(tt.shell, fakeLookPath(tt.present...))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWith: %v", err)
			}
			if got.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Ext != tt.wantExt {
				t.Errorf("ext = %q, want %q", got.Ext, tt.wantExt)
			}
			if strings.Join(got.Args, " ") != strings.Join(tt.wantArgs, " ") {
				t.Errorf("args = %v, want %v", got.Args, tt.wantArgs)
			}
		})
	}
}

func TestResolveErrorNamesCandidates(t *testing.T) {
	_, err := resolveWith("python", fakeLookPath())
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "python3") {
		t.Errorf("error %q should name the candidates tried", err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		t.Error("lookup error should be summarised, not passed through")
	}
}

func TestInterpArgv(t *testing.T) {
	i := Interp{Path: "/usr/bin/pwsh", Args: []string{"-NoProfile", "-File"}}
	got := i.Argv("/tmp/x/script.ps1")
	want := []string{"/usr/bin/pwsh", "-NoProfile", "-File", "/tmp/x/script.ps1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Argv = %v, want %v", got, want)
	}
}
