//go:build windows

package netcert

import (
	"os"
	"path/filepath"
)

// newDeviceKey makes the renewal key inside a CNG key storage provider.
//
// The private half never exists as bytes this process can see, which is the
// point. Two things follow from that and both are visible below.
//
// The key must be PERSISTED before it can sign, so unlike the Unix path there
// is something durable in existence before the certificate is. keyContainerName
// says how that is kept bounded, and discard is what removes it.
//
// Nothing writes network.key. The certificate and chain are still written to
// disk, because that is where the renewal schedule reads the expiry and serial
// from, but the credential the Windows supplicant actually presents comes from
// the machine certificate store, which is what save installs into.
func newDeviceKey(dir, deviceID, predecessorSerial string) (*deviceKey, error) {
	name := keyContainerName(deviceID, predecessorSerial)
	signer, err := createCNGKey(name)
	if err != nil {
		return nil, err
	}
	return &deviceKey{
		signer: signer,
		save: func(certPEM, chainPEM string) (*Material, error) {
			// The store first. A certificate on disk that is not in the store is
			// one the supplicant cannot see, and the renewal loop would read the
			// files, find a fresh expiry, and report a healthy device that
			// cannot authenticate.
			if err := installCertificate(name, certPEM, chainPEM); err != nil {
				return nil, err
			}
			signer.Close()
			m, err := Save(dir, certPEM, chainPEM)
			if err != nil {
				return nil, err
			}
			// A machine upgraded from the file-based build still has the old
			// network.key sitting there, and leaving it would mean this change
			// removed the key from disk for new installs and not for the ones
			// that have been running longest.
			//
			// Only now, after the replacement is installed and saved: until
			// this point the old key and its certificate are still the pair the
			// device holds, and deleting one half of it early is how a renewal
			// leaves a device worse off than not renewing.
			//
			// Not reported if it fails, and that is not laziness. An error out
			// of save sends Ensure into discard, which deletes the container the
			// certificate now in the store is bound to. Failing a renewal that
			// worked, in order to complain that a superseded file is still
			// there, would be a very expensive way to be tidy.
			_ = os.Remove(filepath.Join(dir, keyFileName))
			return m, nil
		},
		discard: func() error {
			signer.Close()
			return deleteCNGKey(name)
		},
	}, nil
}
