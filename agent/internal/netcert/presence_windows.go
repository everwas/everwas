//go:build windows

package netcert

import (
	"bytes"
	"crypto/sha1"
	"crypto/x509"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Presence confirms this device holds a network certificate where ITS
// supplicant will look for one, and names the CA that issued it.
//
// On Windows "where the supplicant looks" is LocalMachine\My, not the netcert
// directory. The key never exists as a file at all (see key_windows.go), and
// checking the directory here is exactly the mistake this replaces: a device
// whose certificate sat in the store, HasPrivateKey True, ready to
// authenticate, was told it held no network certificate because a directory
// the Windows supplicant never reads was empty.
//
// The issuer thumbprint is the SHA-1 of OUR issuing CA's certificate, read
// back from the CA/Root stores installCertificate put the chain in. It is
// what the profile pins the client certificate with, and deriving it from the
// same store the supplicant will search means the pin can never name a CA the
// machine does not hold. SHA-1 because a Windows thumbprint IS the SHA-1 of
// the DER; it identifies a certificate, it does not protect one.
func Presence(dir, agentID string) (issuerThumbprint string, err error) {
	// dir is where the Unix build keeps its PEM files. Unused here, and kept
	// in the signature so the caller does not need a platform branch.
	_ = dir

	leaf, err := storedDeviceCert(agentID)
	if err != nil {
		return "", err
	}
	issuer, err := storedIssuerOf(leaf)
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(issuer.Raw)
	return fmt.Sprintf("%x", sum), nil
}

// storedDeviceCert finds THIS device's certificate in LocalMachine\My: the one
// whose Common Name is the device id and that is bound to a private key.
//
// The key binding is part of the match, not a nicety. A certificate without
// CERT_KEY_PROV_INFO looks entirely correct in certutil and the supplicant
// silently will not use it (see certKeyProvInfoPropID), so treating one as
// present would write a profile for a credential that does not exist.
func storedDeviceCert(agentID string) (*x509.Certificate, error) {
	var found *x509.Certificate
	err := eachStoredCert(storeMy, func(ctx *windows.CertContext, cert *x509.Certificate) bool {
		if cert.Subject.CommonName != agentID || contextKeyContainer(ctx) == "" {
			return true
		}
		found = cert
		return false
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf(
			"%w: nothing in LocalMachine\\My has CN=%s with a private key", ErrNoCertificate, agentID)
	}
	return found, nil
}

// storedIssuerOf finds the CA that signed leaf, in the stores installChain
// filled: CA for intermediates, Root when the leaf was signed by the root
// directly.
//
// Matched by signature, not only by name. Two CAs can share a subject string,
// and pinning the one that merely claims the name would produce a profile
// that filters our certificate OUT, which reads as an authentication
// rejection with nothing pointing here.
func storedIssuerOf(leaf *x509.Certificate) (*x509.Certificate, error) {
	for _, name := range []string{storeCA, storeRoot} {
		var found *x509.Certificate
		err := eachStoredCert(name, func(_ *windows.CertContext, cert *x509.Certificate) bool {
			if !bytes.Equal(cert.RawSubject, leaf.RawIssuer) ||
				leaf.CheckSignatureFrom(cert) != nil {
				return true
			}
			found = cert
			return false
		})
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, fmt.Errorf(
		"netcert: the CA that issued this device's certificate (%s) is not in LocalMachine\\CA or Root, so a supplicant could not build a chain from it either",
		leaf.Issuer)
}

// eachStoredCert walks one LocalMachine store read-only, stopping when fn
// returns false. Certificates that do not parse are skipped rather than
// fatal: the machine's stores hold whatever anyone ever put there, and one
// broken stranger must not make ours unfindable.
func eachStoredCert(storeName string, fn func(ctx *windows.CertContext, cert *x509.Certificate) bool) error {
	store, err := readMachineStore(storeName)
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)

	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			return nil
		}
		prev = ctx
		cert, err := parseContext(ctx)
		if err != nil {
			continue
		}
		if !fn(ctx, cert) {
			return nil
		}
	}
}

// readMachineStore opens a LocalMachine store for reading only. Distinct from
// openMachineStore because that one asks for write access, and this path runs
// from a command an operator may not have elevated: looking at what the
// machine holds must not require the right to change it.
func readMachineStore(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("netcert: store name: %w", err)
	}
	h, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_READONLY_FLAG|windows.CERT_STORE_OPEN_EXISTING_FLAG,
		uintptr(unsafe.Pointer(namePtr)),
	)
	if err != nil {
		return 0, fmt.Errorf("netcert: open LocalMachine\\%s: %w", name, err)
	}
	return h, nil
}
