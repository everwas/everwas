//go:build !windows

package update

// BackupPath is where Swap parks the outgoing binary. On unix the running
// executable can be renamed or even unlinked while it runs, so a sibling
// ".old" file on the same filesystem is all we need.
func BackupPath(target string) string { return target + ".old" }

// SpawnFinalizer exists so callers can be platform agnostic. Unix never needs
// a helper process: the swap completes in place and the service manager
// restarts the agent after it exits.
func SpawnFinalizer(staged, target string) error { return ErrNoFinalizer }

// NeedsFinalizer reports whether SpawnFinalizer is a usable fallback here.
func NeedsFinalizer() bool { return false }
