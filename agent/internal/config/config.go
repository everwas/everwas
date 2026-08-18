// Package config persists the agent's identity in a JSON state file
// (agent.json) inside the OS state directory.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/everwas/everwas/agent/internal/secure"
)

// FileName is the state file inside Dir().
const FileName = "agent.json"

// Config is everything the agent needs to reconnect after a restart.
type Config struct {
	ServerURL   string `json:"server_url"`
	AgentID     string `json:"agent_id"`
	AgentSecret string `json:"agent_secret"`
	NATSURL     string `json:"nats_url"`
}

// Enrolled reports whether the config carries usable credentials.
func (c *Config) Enrolled() bool {
	return c != nil && c.AgentID != "" && c.AgentSecret != ""
}

// Dir returns the OS state directory. $EVERWAS_STATE_DIR overrides everything
// (dev and test escape hatch); on Linux a non-root agent falls back to
// ~/.config/everwas so enrollment works without sudo.
func Dir() (string, error) {
	if d := os.Getenv("EVERWAS_STATE_DIR"); d != "" {
		return d, nil
	}
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() == 0 {
			return "/etc/everwas", nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, ".config", "everwas"), nil
	case "darwin":
		return "/Library/Application Support/Everwas", nil
	case "windows":
		return `C:\ProgramData\Everwas\Agent`, nil
	default:
		return "", fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// Path returns the full path of the state file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the state file from the default location. A missing file is not
// an error: it returns an empty (not yet enrolled) config.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the state file at path.
func LoadFrom(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config to the default location.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return c.SaveTo(path)
}

// SaveTo writes the config to path, rename-into-place so a crash mid-write
// never leaves a truncated state file.
//
// The directory is created through secure rather than os.MkdirAll, because
// this file holds the agent secret. The 0700 that used to be here was honoured
// on Linux and ignored on Windows, where the directory inherited
// C:\ProgramData's default and left the credential readable by every local
// user on the machine.
func (c *Config) SaveTo(path string) error {
	if err := secure.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
