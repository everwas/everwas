//go:build !windows

package secure

import "os"

// harden tightens an EXISTING directory to 0700.
//
// The chmod is not redundant with the 0700 passed to os.MkdirAll. MkdirAll
// applies its mode only to directories it actually creates and returns nil
// without touching one that is already there, so on an agent installed before
// this existed the mode argument alone repairs nothing: the directory stays
// exactly as readable as it was, and the call reads as though it had fixed it.
//
// The umask also applies to MkdirAll's mode, so an unusual umask could leave a
// freshly created directory looser than asked for. Chmod is not subject to it.
func harden(path string) error {
	return os.Chmod(path, 0o700)
}
