//go:build !linux && !windows && !darwin

package patch

// Detect has no backend to offer on this platform. The agent still runs;
// patch jobs fail with a clear reason instead of silently reporting a
// fully patched host.
func Detect() (Manager, error) { return nil, ErrUnsupported }
