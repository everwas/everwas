//go:build !windows

package scripts

// defaultShell is what "auto" resolves to on unix. bash is the table entry,
// which itself falls back to sh when bash is absent.
func defaultShell() string { return "bash" }

func interpTable() map[string]spec {
	return map[string]spec{
		"bash":   {candidates: []string{"bash", "sh"}, ext: ".sh"},
		"sh":     {candidates: []string{"sh"}, ext: ".sh"},
		"zsh":    {candidates: []string{"zsh"}, ext: ".sh"},
		"python": {candidates: []string{"python3", "python"}, ext: ".py"},
		// pwsh on unix has no -ExecutionPolicy; passing it is an error.
		"pwsh":       {candidates: []string{"pwsh"}, args: []string{"-NoProfile", "-NonInteractive", "-File"}, ext: ".ps1"},
		"powershell": {candidates: []string{"pwsh", "powershell"}, args: []string{"-NoProfile", "-NonInteractive", "-File"}, ext: ".ps1"},
	}
}
