package supplicant

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/everwas/everwas/agent/internal/secure"
)

// FileName is the generated profile inside the directory given to Write.
const FileName = "wpa_supplicant-everwas.conf"

// Write renders the profile and writes it, replacing any previous one.
//
// Returns the path written. It does NOT start or reload a supplicant: see the
// package comment for why applying is deliberately a separate, human decision.
//
// Written through a temporary file and renamed into place, because a
// supplicant reading this file while it is half-written gets a config that
// parses to something other than what either side intended, and the failure
// appears as an authentication problem rather than as a truncated file.
func Write(dir string, p Profile) (string, error) {
	content, err := Render(p)
	if err != nil {
		return "", err
	}
	// The directory is created with the same protection as the rest of the
	// agent's state. The profile itself holds no secret, but it names the path
	// of the private key and the identity this machine authenticates as, and
	// neither is worth handing to every local user.
	if err := secure.MkdirAll(dir); err != nil {
		return "", err
	}

	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("supplicant: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("supplicant: install %s: %w", path, err)
	}
	return path, nil
}
