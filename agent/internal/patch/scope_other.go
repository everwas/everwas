//go:build !linux

package patch

// scopeCommand has nothing to wrap outside systemd. macOS softwareupdate and
// the Windows Update Agent both survive the agent restarting on their own.
func scopeCommand(name string, args []string) (string, []string, bool) {
	return name, args, false
}
