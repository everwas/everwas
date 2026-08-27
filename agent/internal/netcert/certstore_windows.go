//go:build windows

package netcert

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The three machine stores this touches. Named rather than spelled inline
// because "My" reads like a typo at the call site.
const (
	storeMy   = "My"
	storeCA   = "CA"
	storeRoot = "Root"
)

const (
	// certKeyProvInfoPropID is CERT_KEY_PROV_INFO_PROP_ID, the property that
	// tells Windows WHICH key container holds the private half of this
	// certificate.
	//
	// The single most important line in this file. A certificate added to the
	// store without it looks entirely correct in certutil and in the MMC
	// snap-in, shows the right subject and the right expiry, and the supplicant
	// silently will not use it, because as far as Windows is concerned the
	// machine holds somebody else's certificate.
	certKeyProvInfoPropID = 2

	// cryptMachineKeyset says the container named below lives in the machine
	// key store, matching ncryptMachineKeyFlag on the way in. Without it
	// Windows looks for the container under the calling user and does not find
	// it.
	cryptMachineKeyset = 0x00000020
)

// cryptKeyProvInfo is CRYPT_KEY_PROV_INFO. Not bound by
// golang.org/x/sys/windows, so it is declared here.
//
// The field order and the padding matter: this struct is handed to crypt32 as
// raw memory. Go inserts four bytes after KeySpecPad on amd64 exactly where the
// C compiler does, because the following member is pointer-aligned.
type cryptKeyProvInfo struct {
	ContainerName *uint16
	ProvName      *uint16
	ProvType      uint32
	Flags         uint32
	ProvParam     uint32
	RgProvParam   uintptr
	// KeySpec is zero for a CNG key. The AT_KEYEXCHANGE and AT_SIGNATURE values
	// belong to the older CryptoAPI providers, and setting one of them here
	// sends Windows looking for a legacy container that does not exist.
	KeySpec uint32
}

var (
	crypt32DLL = windows.NewLazySystemDLL("crypt32.dll")

	// x/sys/windows binds the store and context calls but not the two property
	// ones, which is unfortunate given that the property is what makes the
	// difference between a certificate that works and one that only looks like
	// it does.
	procCertSetCertificateContextProperty = crypt32DLL.NewProc("CertSetCertificateContextProperty")
	procCertGetCertificateContextProperty = crypt32DLL.NewProc("CertGetCertificateContextProperty")
)

// installCertificate puts the issued certificate in the machine store, bound to
// the container holding its key, and puts the chain where path building will
// find it.
//
// container is the CNG container name from keyContainerName. It is not enough
// for the certificate and the key to both exist: the binding between them is a
// separate property, set below.
func installCertificate(container, certPEM, chainPEM string) error {
	leafDER, leaf, err := firstCert(certPEM)
	if err != nil {
		return fmt.Errorf("netcert: certificate: %w", err)
	}

	my, err := openMachineStore(storeMy)
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(my, 0)

	if err := addToStore(my, leafDER, container); err != nil {
		return err
	}
	if err := installChain(chainPEM, leaf); err != nil {
		return err
	}

	// The predecessor and its key container. Swept AFTER the replacement is in
	// place, so a machine that dies mid-renewal is left with two working
	// certificates rather than none.
	//
	// Failures here are swallowed on purpose. The renewal has already
	// succeeded, the device can authenticate, and turning "could not tidy up
	// the old certificate" into a returned error would fail a renewal that
	// worked, which is a much worse outcome than a stale entry in the store.
	sweepSuperseded(my, leaf)
	return nil
}

// openMachineStore opens one of the LocalMachine system stores for writing.
func openMachineStore(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("netcert: store name: %w", err)
	}
	// CERT_SYSTEM_STORE_LOCAL_MACHINE, not the current user's. The supplicant
	// authenticates the machine, at the login screen, with nobody signed in.
	h, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE,
		uintptr(unsafe.Pointer(namePtr)),
	)
	if err != nil {
		return 0, fmt.Errorf("netcert: open LocalMachine\\%s: %w", name, err)
	}
	return h, nil
}

// addToStore adds one certificate and, when container is non-empty, binds it to
// its key.
func addToStore(store windows.Handle, der []byte, container string) error {
	ctx, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING, &der[0], uint32(len(der)))
	if err != nil {
		return fmt.Errorf("netcert: build certificate context: %w", err)
	}
	defer windows.CertFreeCertificateContext(ctx)

	// REPLACE_EXISTING rather than ADD_NEW: a device that reinstalls or comes
	// back from an image can be handed the same certificate twice, and ADD_NEW
	// would fail the second time for no reason anyone benefits from.
	var stored *windows.CertContext
	if err := windows.CertAddCertificateContextToStore(
		store, ctx, windows.CERT_STORE_ADD_REPLACE_EXISTING, &stored,
	); err != nil {
		return fmt.Errorf("netcert: add certificate to store: %w", err)
	}
	defer windows.CertFreeCertificateContext(stored)

	if container == "" {
		return nil
	}

	// Set the property on the context the STORE handed back, not on the
	// in-memory one built above. That is the copy that persists.
	containerPtr, err := windows.UTF16PtrFromString(container)
	if err != nil {
		return fmt.Errorf("netcert: container name: %w", err)
	}
	provPtr, err := windows.UTF16PtrFromString(keyStorageProvider)
	if err != nil {
		return fmt.Errorf("netcert: provider name: %w", err)
	}
	info := cryptKeyProvInfo{
		ContainerName: containerPtr,
		ProvName:      provPtr,
		// ProvType zero means a CNG provider, named by ProvName. A non-zero
		// value here selects a legacy CryptoAPI provider type instead.
		ProvType: 0,
		Flags:    cryptMachineKeyset,
		KeySpec:  0,
	}
	r, _, e := syscall.SyscallN(procCertSetCertificateContextProperty.Addr(),
		uintptr(unsafe.Pointer(stored)), certKeyProvInfoPropID, 0,
		uintptr(unsafe.Pointer(&info)),
	)
	// The struct is passed by pointer and the two strings hang off it, so all
	// three have to outlive the call. Only the struct is reachable from the
	// argument list as far as the compiler is concerned.
	runtime.KeepAlive(containerPtr)
	runtime.KeepAlive(provPtr)
	if r == 0 {
		return fmt.Errorf("netcert: bind certificate to key container %s: %w", container, e)
	}
	return nil
}

// installChain places the issuing certificates where Windows will look for them
// when it builds the path the supplicant presents.
//
// Intermediates go to CA and the self-signed root goes to Root. Putting the
// root there is a real widening: that CA becomes trusted for the whole machine,
// for everything, not just for this. It is accepted because a supplicant that
// cannot build a complete path sends a partial one and the RADIUS server
// rejects it, and the rejection names the certificate rather than the missing
// link. Worth knowing when reading the profile's own server validation, which
// is a SEPARATE trust decision: with no TrustedRootCA pinned there, anything
// chaining to any machine-trusted root is accepted as the server, and this
// makes that set one root larger.
func installChain(chainPEM string, leaf *x509.Certificate) error {
	rest := []byte(chainPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("netcert: chain: %w", err)
		}
		// Servers commonly include the leaf in the chain they return. Adding it
		// to CA or Root would make the device trust itself, which is harmless
		// and confusing, and confusing is enough reason not to.
		if cert.Equal(leaf) {
			continue
		}

		name := storeCA
		if isSelfSigned(cert) {
			name = storeRoot
		}
		store, err := openMachineStore(name)
		if err != nil {
			return err
		}
		// No container: these are somebody else's certificates and this machine
		// holds no key for them.
		err = addToStore(store, block.Bytes, "")
		windows.CertCloseStore(store, 0)
		if err != nil {
			return err
		}
	}
}

func isSelfSigned(cert *x509.Certificate) bool {
	return cert.CheckSignatureFrom(cert) == nil
}

// sweepSuperseded removes earlier certificates for this device and the key
// containers they were bound to.
//
// Without it every successful renewal leaves the previous certificate in My and
// its key in the provider, so the accumulation problem simply moves from the
// failure path to the success path and arrives more slowly.
//
// Matching is by subject and by NOT being the certificate just installed. The
// subject is the device id, set by the server from the credential the agent
// authenticated with, so nothing else on the machine carries it.
func sweepSuperseded(store windows.Handle, keep *x509.Certificate) {
	// Two passes. CertDeleteCertificateFromStore frees the context it is given,
	// which is also the enumeration's position, so deleting inside the loop
	// walks off the end of a freed list. The first pass duplicates the matches
	// and lets the enumeration finish.
	var doomed []*windows.CertContext
	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			break
		}
		prev = ctx
		cert, err := parseContext(ctx)
		if err != nil {
			continue
		}
		if cert.Subject.String() != keep.Subject.String() || cert.Equal(keep) {
			continue
		}
		doomed = append(doomed, windows.CertDuplicateCertificateContext(ctx))
	}

	for _, ctx := range doomed {
		container := contextKeyContainer(ctx)
		// Delete the certificate first. If the process dies between the two,
		// what is left is an orphaned container, which the next renewal's
		// deterministic naming keeps bounded. The other order leaves a
		// certificate in the store pointing at a key that is gone, which the
		// supplicant will try to use.
		_ = windows.CertDeleteCertificateFromStore(ctx)
		// Only containers this package created. A certificate that arrived by
		// some other route may name a container something else depends on.
		if strings.HasPrefix(container, keyContainerPrefix) {
			_ = deleteCNGKey(container)
		}
	}
}

// contextKeyContainer reads the container name a stored certificate is bound
// to, or "" if it is bound to nothing.
func contextKeyContainer(ctx *windows.CertContext) string {
	var size uint32
	r, _, _ := syscall.SyscallN(procCertGetCertificateContextProperty.Addr(),
		uintptr(unsafe.Pointer(ctx)), certKeyProvInfoPropID, 0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 || size == 0 {
		return ""
	}
	buf := make([]byte, size)
	r, _, _ = syscall.SyscallN(procCertGetCertificateContextProperty.Addr(),
		uintptr(unsafe.Pointer(ctx)), certKeyProvInfoPropID,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	// The struct sits at the front of the buffer and its string pointers point
	// further into that same buffer, so the name has to be copied out before
	// buf can be collected.
	info := (*cryptKeyProvInfo)(unsafe.Pointer(&buf[0]))
	name := windows.UTF16PtrToString(info.ContainerName)
	runtime.KeepAlive(buf)
	return name
}

func parseContext(ctx *windows.CertContext) (*x509.Certificate, error) {
	if ctx == nil || ctx.EncodedCert == nil || ctx.Length == 0 {
		return nil, errors.New("netcert: empty certificate context")
	}
	der := unsafe.Slice(ctx.EncodedCert, ctx.Length)
	// Copied: the DER belongs to the context and x509.ParseCertificate keeps
	// references into whatever it is handed.
	return x509.ParseCertificate(append([]byte(nil), der...))
}

// firstCert returns the DER and the parsed form of the first certificate in a
// PEM bundle.
func firstCert(certPEM string) ([]byte, *x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return block.Bytes, cert, nil
}
