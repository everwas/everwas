// Package svc installs, removes and inspects the agent's system service. It
// speaks each platform's native mechanism directly (systemd unit, launchd
// daemon, Windows SCM) rather than pulling in a cross platform service
// library, so the generated artifacts are the ones an operator would have
// written by hand and can edit afterwards.
package svc

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Identity of the service across platforms.
const (
	// Name is the systemd unit name and the Windows service name.
	Name = "everwas-agent"
	// DisplayName is what a human sees in a service list.
	DisplayName = "Everwas Agent"
	// Description is the one line summary shown by systemctl and services.msc.
	Description = "Everwas remote monitoring and management agent"
	// LaunchdLabel is the macOS daemon label.
	LaunchdLabel = "systems.supported.everwas.agent"
	// MacLogDir holds the launchd stdout and stderr files.
	MacLogDir = "/Library/Logs/Everwas"

	// PrefixEnv relocates every path this package writes. It exists so tests
	// and packaging dry runs can never touch a real /etc or /Library.
	PrefixEnv = "EVERWAS_SERVICE_PREFIX"
	// StateDirEnv is the agent wide state directory override. When it is set
	// at install time the value is baked into the service definition so the
	// running service uses the same directory the installer did.
	StateDirEnv = "EVERWAS_STATE_DIR"
)

// Errors returned by the platform implementations.
var (
	ErrUnsupported  = errors.New("svc: service management is not supported on this OS")
	ErrNotInstalled = errors.New("svc: service is not installed")
	ErrNoBinary     = errors.New("svc: binary path is required")
)

// Status values. Platform implementations map their native states onto these
// so callers can compare without knowing which OS they are on.
const (
	StatusRunning      = "running"
	StatusStopped      = "stopped"
	StatusNotInstalled = "not installed"
	StatusUnknown      = "unknown"
)

// InstallConfig describes the service to create.
type InstallConfig struct {
	// BinaryPath is the absolute path of the installed agent binary.
	BinaryPath string
	// Args are the arguments passed to the binary. Defaults to ["run"].
	Args []string
	// StateDir, when set, is exported to the service as EVERWAS_STATE_DIR.
	StateDir string
	// Prefix relocates the service definition files. Empty means the real
	// system location. Tests set it; installs do not.
	Prefix string
}

// normalized fills in defaults without mutating the caller's value.
func (c InstallConfig) normalized() InstallConfig {
	if len(c.Args) == 0 {
		c.Args = []string{"run"}
	}
	if c.Prefix == "" {
		c.Prefix = os.Getenv(PrefixEnv)
	}
	if c.StateDir == "" {
		c.StateDir = os.Getenv(StateDirEnv)
	}
	return c
}

func (c InstallConfig) validate() error {
	if strings.TrimSpace(c.BinaryPath) == "" {
		return ErrNoBinary
	}
	return nil
}

// DefaultInstallPath is where `everwas-agent install` puts the binary.
func DefaultInstallPath() string {
	switch runtime.GOOS {
	case "linux":
		return "/usr/local/bin/everwas-agent"
	case "darwin":
		return "/Library/Everwas/Agent/everwas-agent"
	case "windows":
		return `C:\Program Files\Everwas\Agent\everwas-agent.exe`
	default:
		return ""
	}
}

// UnitPath is the systemd unit location, honouring the prefix override.
func UnitPath(prefix string) string {
	return rooted(prefix, "etc", "systemd", "system", Name+".service")
}

// PlistPath is the launchd daemon location, honouring the prefix override.
func PlistPath(prefix string) string {
	return rooted(prefix, "Library", "LaunchDaemons", LaunchdLabel+".plist")
}

// rooted joins elements under prefix, falling back to the filesystem root so
// an empty prefix yields the real absolute system path.
func rooted(prefix string, elem ...string) string {
	if prefix == "" {
		prefix = string(filepath.Separator)
	}
	return filepath.Join(append([]string{prefix}, elem...)...)
}

// prefixOrEnv resolves the relocation prefix for the read-only entry points
// (Uninstall, Status) that do not take an InstallConfig.
func prefixOrEnv() string { return os.Getenv(PrefixEnv) }

// writeFileAtomic writes a service definition through a temp file so a
// half-written unit can never be picked up by a daemon-reload.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, mode)
}
