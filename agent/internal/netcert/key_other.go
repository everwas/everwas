//go:build !windows

package netcert

// newDeviceKey makes the renewal key in MEMORY and writes nothing.
//
// deviceID and predecessorSerial are unused here, and that is the point rather
// than an oversight: a key that has not been written has no name, so there is
// no generation to track and nothing that a second attempt could collide with.
// They exist for Windows, where the key has to be inside the provider before it
// can sign anything. See keyContainerName.
func newDeviceKey(dir, deviceID, predecessorSerial string) (*deviceKey, error) {
	key, keyPEM, err := newKeyPair()
	if err != nil {
		return nil, err
	}
	return &deviceKey{
		signer: key,
		save: func(certPEM, chainPEM string) (*Material, error) {
			return saveAll(dir, keyPEM, certPEM, chainPEM)
		},
		// Nothing was made durable, so there is nothing to undo. The key is
		// unreferenced the moment this deviceKey goes out of scope.
		discard: func() error { return nil },
	}, nil
}
