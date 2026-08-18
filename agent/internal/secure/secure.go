// Package secure creates directories that only the machine's administrators
// can read.
//
// It exists because os.MkdirAll(path, 0o700) does NOT mean that on Windows.
// The mode argument is very nearly ignored there: the directory inherits its
// parent's ACL, and the parent for an agent is C:\ProgramData, whose default
// grants BUILTIN\Users read and write. Everything the agent had written under
// it was therefore readable by every local user on the box, including
// agent.json, which holds the credential that authenticates this device to the
// server, and the 802.1X private key, which is the machine's identity on the
// network for the life of its certificate.
//
// Nothing about that was visible in the Go source. The call had a 0700 in it
// and a comment saying dir 0700, file 0600, and both were true on Linux, which
// is where it was tested.
package secure

import (
	"fmt"
	"os"
)

// MkdirAll creates a directory readable only by the local administrators.
//
// On Unix that is 0700 and the mode does the work. On Windows the mode is
// ignored, so the directory is given an explicit protected ACL granting SYSTEM
// and BUILTIN\Administrators and nobody else.
//
// Applied on every call, not just at creation. An agent that was installed
// before this existed already has a world-readable directory full of secrets,
// and it must be repaired without needing a reinstall.
func MkdirAll(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("secure: create %s: %w", path, err)
	}
	if err := harden(path); err != nil {
		return fmt.Errorf("secure: restrict %s: %w", path, err)
	}
	return nil
}
