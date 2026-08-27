//go:build !windows

package winident

import "context"

// detect finds nothing off Windows, which is the truthful answer rather than a
// stub: there is no Active Directory machine certificate store here to compete
// with, so nothing can be providing a competing identity.
func detect(context.Context) (Sources, error) { return Sources{}, nil }
