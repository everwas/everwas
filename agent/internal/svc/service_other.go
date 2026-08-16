//go:build !linux && !darwin && !windows

package svc

// The agent only ships for linux, darwin and windows. These stubs keep the
// package buildable on anything else so a `go build ./...` on an unusual
// GOOS fails at the right place with a readable message.

// Install is not supported on this OS.
func Install(cfg InstallConfig) error { return ErrUnsupported }

// Uninstall is not supported on this OS.
func Uninstall() error { return ErrUnsupported }

// Status is not supported on this OS.
func Status() (string, error) { return StatusUnknown, ErrUnsupported }

// Start is not supported on this OS.
func Start() error { return ErrUnsupported }

// Stop is not supported on this OS.
func Stop() error { return ErrUnsupported }

// Restart is not supported on this OS.
func Restart() error { return ErrUnsupported }
