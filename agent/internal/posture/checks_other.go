//go:build !linux && !windows

package posture

// Checks is empty on platforms with no checks written yet.
//
// Empty rather than absent, so the collector runs and reports "nothing
// assessed" instead of failing to build. macOS belongs here and is deferred:
// FileVault, the application firewall and XProtect each need their own file,
// and a half-written check that returns the wrong answer is worse than an
// honest empty list.
func Checks() []Check { return nil }
