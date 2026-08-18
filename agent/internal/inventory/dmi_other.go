//go:build !linux && !windows && !darwin

package inventory

import "context"

// Platforms without a DMI story report nothing, and the hardware snapshot
// omits the identity fields entirely (omitempty). Empty is honest.
func collectDMI(_ context.Context) dmiInfo {
	return dmiInfo{}
}
