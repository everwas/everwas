//go:build windows

package scripts

func defaultShell() string { return "powershell" }

func interpTable() map[string]spec {
	psArgs := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}
	return map[string]spec{
		"powershell": {candidates: []string{"powershell.exe", "pwsh.exe"}, args: psArgs, ext: ".ps1"},
		"pwsh":       {candidates: []string{"pwsh.exe", "powershell.exe"}, args: psArgs, ext: ".ps1"},
		"cmd":        {candidates: []string{"cmd.exe"}, args: []string{"/c"}, ext: ".cmd"},
		"python":     {candidates: []string{"python3.exe", "python.exe", "python3", "python"}, ext: ".py"},
		// Present on hosts with Git for Windows or WSL; absent otherwise,
		// which Resolve reports as a clean "not found in PATH".
		"bash": {candidates: []string{"bash.exe", "bash"}, ext: ".sh"},
		"sh":   {candidates: []string{"sh.exe", "sh"}, ext: ".sh"},
	}
}
