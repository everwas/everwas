package shell

// PTY is the platform-independent view of a pseudo-terminal running a shell.
// The unix (creack/pty) and windows (ConPTY) implementations both satisfy it,
// so session.go carries no build tags.
type PTY interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Close() error
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
}

// ptyEnv is the environment handed to every shell session. TERM matters:
// without it the remote shell assumes a dumb terminal and the browser's
// xterm.js renders control sequences as garbage.
func ptyEnv(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if len(kv) >= 5 && kv[:5] == "TERM=" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color")
}
