package scripts

import (
	"fmt"
	"os/exec"
	"strings"
)

// Interp is a resolved interpreter: the executable to run, the arguments
// that precede the script path, and the extension the script file needs.
type Interp struct {
	Shell string   // canonical shell name after alias resolution
	Path  string   // absolute path to the interpreter
	Args  []string // arguments before the script path
	Ext   string   // script file extension, including the dot
}

// Argv returns the full command line for running scriptPath.
func (i Interp) Argv(scriptPath string) []string {
	return append(append([]string{i.Path}, i.Args...), scriptPath)
}

// spec is one row of the per-OS interpreter table.
type spec struct {
	candidates []string // executables tried in order
	args       []string
	ext        string
}

type lookupFunc func(string) (string, error)

// Resolve finds the interpreter for a shell name as sent by the server.
// "" and "auto" mean "whatever this OS runs scripts with".
func Resolve(shell string) (Interp, error) {
	return resolveWith(shell, exec.LookPath)
}

func resolveWith(shell string, look lookupFunc) (Interp, error) {
	name := canonicalShell(shell)
	sp, ok := interpTable()[name]
	if !ok {
		return Interp{}, fmt.Errorf("unsupported shell %q", shell)
	}
	for _, c := range sp.candidates {
		path, err := look(c)
		if err != nil {
			continue
		}
		return Interp{Shell: name, Path: path, Args: sp.args, Ext: sp.ext}, nil
	}
	return Interp{}, fmt.Errorf("no interpreter for %q found in PATH (tried %s)",
		name, strings.Join(sp.candidates, ", "))
}

// canonicalShell folds aliases so the table stays small.
func canonicalShell(shell string) string {
	s := strings.ToLower(strings.TrimSpace(shell))
	switch s {
	case "", "auto", "default":
		return defaultShell()
	case "python3", "py":
		return "python"
	case "powershell.exe":
		return "powershell"
	case "pwsh.exe":
		return "pwsh"
	case "cmd.exe", "bat", "batch":
		return "cmd"
	default:
		return s
	}
}
