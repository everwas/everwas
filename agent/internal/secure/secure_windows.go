package secure

import (
	"golang.org/x/sys/windows"
)

// protectedDACL grants SYSTEM and the local Administrators group full control
// and nobody else, and inherits down to everything created inside.
//
//	D:PAI  - a DACL that is Protected, so the permissive ACEs inherited from
//	         C:\ProgramData are dropped rather than merged. Without the P this
//	         adds our entries and leaves BUILTIN\Users read intact, which looks
//	         correct in a diff and fixes nothing.
//	OICI   - object and container inherit: applies to files and subdirectories
//	         created later, so netcert/ and work/ do not each need this again.
//	FA     - full access.
//	SY, BA - NT AUTHORITY\SYSTEM (the account the service runs as) and
//	         BUILTIN\Administrators (so an operator can still read the state
//	         dir to debug, which they can do anyway by taking ownership).
const protectedDACL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// harden replaces path's DACL and lets Windows propagate it to the children.
//
// The propagation is the point: an agent upgraded from a build without this
// already has files sitting under the inherited permissive ACL, and setting
// the parent alone would leave them exactly as readable as before.
func harden(path string) error {
	sd, err := windows.SecurityDescriptorFromString(protectedDACL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}
