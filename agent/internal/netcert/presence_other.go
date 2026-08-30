//go:build !windows

package netcert

// Presence confirms this device holds a network certificate where ITS
// supplicant will look for one, and names the CA that issued it when the
// platform needs that name.
//
// On Unix "where the supplicant looks" is the PEM files in dir, because
// wpa_supplicant is handed their paths, so the file check is the honest one:
// a profile naming files that are not there fails the handshake with an error
// about the certificate, which sends whoever reads it looking at the CA
// rather than at the machine that never obtained one.
//
// The issuer thumbprint is "" here and that is not a gap. wpa_supplicant
// presents exactly the certificate the profile points at by path; there is no
// store full of candidates to pick the wrong one from, so there is nothing to
// pin. The Windows build is the one that has to name an issuer: see
// presence_windows.go.
func Presence(dir, agentID string) (issuerThumbprint string, err error) {
	if _, err := Load(dir); err != nil {
		return "", err
	}
	return "", nil
}
