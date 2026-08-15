package scripts

import (
	"os"
	"path/filepath"
	"strings"
)

// stageScript writes body to a private per-job directory and returns the
// directory and the script path. The directory lives under the agent state
// dir rather than /tmp: a world-readable temp file would leak whatever the
// script body contains (often credentials) to every local user.
func stageScript(stateDir, jobID, body, ext string) (dir, script string, err error) {
	base := stateDir
	if base == "" {
		base = os.TempDir()
	}
	dir = filepath.Join(base, "work", safeName(jobID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	// MkdirAll respects umask, so force the mode we asked for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", err
	}
	script = filepath.Join(dir, "script"+ext)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if err := os.Chmod(script, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, script, nil
}

// safeName makes a job id usable as a directory name on every platform.
// Scheduled job ids look like "sched:{entry}:{ts}" and colons are illegal
// in Windows paths.
func safeName(jobID string) string {
	var b strings.Builder
	for _, r := range jobID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), ".")
	if name == "" {
		name = "job"
	}
	if len(name) > 96 {
		name = name[:96]
	}
	return name
}
