//go:build !windows

package svc

import "context"

// IsService reports whether this process was launched by a service manager
// that expects a control protocol.
//
// Only Windows has one. systemd, launchd and the rest run the binary as an
// ordinary process and read its exit code, which is why this whole seam does
// not exist on those platforms.
func IsService() (bool, error) { return false, nil }

// RunAsService is never called on these platforms. It exists so the caller
// does not need a build tag of its own.
func RunAsService(_ string, work func(context.Context) int) int {
	return work(context.Background())
}
