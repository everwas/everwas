//go:build windows

package update

import "golang.org/x/sys/windows"

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not finished (STILL_ACTIVE / STATUS_PENDING).
const stillActive = 259

// ProcessExited reports whether the process is gone. A handle that cannot be
// opened, or one whose exit code is anything other than STILL_ACTIVE, means
// the old agent has released its executable and the swap can proceed.
func ProcessExited(pid int) bool {
	if pid <= 0 {
		return true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code != stillActive
}
