//go:build windows

package netcert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// keyStorageProvider is the CNG provider the device key is created in.
//
// The Microsoft SOFTWARE provider today. The key is held by LSASS and wrapped
// with DPAPI rather than sitting in a file we wrote, so the agent cannot read
// it back and neither can anyone who takes a copy of the state directory. It is
// still, in the end, protected by software on the same machine.
//
// A constant and not a literal because of its successor: platformCryptoProvider
// is the identical API against the TPM, where the key is generated inside the
// chip and cannot be extracted at all. Everything below works unchanged against
// it, which is the reason this went through CNG rather than the much shorter
// route of generating a key in Go and importing a PKCS#12. That route would
// have had to be written once now and thrown away later.
const keyStorageProvider = "Microsoft Software Key Storage Provider"

// platformCryptoProvider is the TPM-backed provider this moves to. Named here
// so the string exists in one place when the remaining question is answered,
// which is what a machine with no usable TPM should fall back to.
const platformCryptoProvider = "Microsoft Platform Crypto Provider"

// ecdsaP256Algorithm matches the curve the file-based key already used: every
// 802.1X supplicant in use handles it and the handshake is cheap on the switch.
const ecdsaP256Algorithm = "ECDSA_P256"

const (
	// ncryptMachineKeyFlag puts the key in the MACHINE key store rather than in
	// the store of whatever account happened to create it. The agent runs as
	// SYSTEM and the credential is the machine's identity: a per-user key would
	// be invisible to the supplicant, which authenticates at the login screen
	// with nobody signed in.
	ncryptMachineKeyFlag = 0x00000020

	// ncryptSilentFlag forbids the provider from showing UI. A service has no
	// desktop to show it on, so without this a provider that wanted to prompt
	// would block instead of failing.
	ncryptSilentFlag = 0x00000040

	// ncryptOverwriteKeyFlag replaces a container of the same name.
	//
	// Safe here only because of how the name is chosen: it is derived from the
	// certificate being replaced, so it can collide with a previous ATTEMPT at
	// this same renewal and never with the container holding the key that is
	// currently in use. See keyContainerName.
	ncryptOverwriteKeyFlag = 0x00000080
)

// NCRYPT_ALLOW_EXPORT_FLAG is deliberately absent, and its absence is the
// entire point of this file rather than something left out.
//
// A new persisted key defaults to an export policy of zero, meaning the private
// half cannot be exported by any means. exportPolicyProperty is set to zero
// below anyway, explicitly, so that the property in the key states the
// intention rather than merely happening to have the right default. If either
// NCRYPT_ALLOW_EXPORT_FLAG (0x1) or NCRYPT_ALLOW_PLAINTEXT_EXPORT_FLAG (0x2)
// were set here, the key would be bytes again and this would all be theatre.
const (
	exportPolicyProperty = "Export Policy"
	keyUsageProperty     = "Key Usage"

	// allowSigning is NCRYPT_ALLOW_SIGNING_FLAG. This key signs a CSR and then
	// signs 802.1X handshakes. It never wraps anything, so it is not given
	// decryption or key-agreement usage.
	//
	// Two, not one. NCRYPT_ALLOW_DECRYPT_FLAG is 0x1 and signing is 0x2, which
	// is the opposite order to the one they are usually listed in. Getting it
	// wrong produces a key that is created, finalized and stored without
	// complaint, and then fails the first NCryptSignHash with NTE_PERM, an
	// error whose text is "Access denied" and which says nothing about usage.
	allowSigning = 0x00000002
)

// eccPublicBlob is BCRYPT_ECCPUBLIC_BLOB, the only thing this key will export.
const eccPublicBlob = "ECCPUBLICBLOB"

// ecdsaPublicP256Magic is the header a P-256 public blob starts with. Checked
// rather than assumed: if the provider ever hands back a different curve the
// failure should be here and not in a signature that verifies nowhere.
const ecdsaPublicP256Magic = 0x31534345

// Selected SECURITY_STATUS values. The rest are reported as they come back.
const (
	statusSuccess = 0
	// nteBadKeyset is what NCryptOpenKey returns for a container that is not
	// there, which for deletion means the work is already done.
	nteBadKeyset = 0x80090016
)

var (
	ncryptDLL = windows.NewLazySystemDLL("ncrypt.dll")

	procOpenStorageProvider = ncryptDLL.NewProc("NCryptOpenStorageProvider")
	procCreatePersistedKey  = ncryptDLL.NewProc("NCryptCreatePersistedKey")
	procSetProperty         = ncryptDLL.NewProc("NCryptSetProperty")
	procFinalizeKey         = ncryptDLL.NewProc("NCryptFinalizeKey")
	procSignHash            = ncryptDLL.NewProc("NCryptSignHash")
	procExportKey           = ncryptDLL.NewProc("NCryptExportKey")
	procOpenKey             = ncryptDLL.NewProc("NCryptOpenKey")
	procDeleteKey           = ncryptDLL.NewProc("NCryptDeleteKey")
	procFreeObject          = ncryptDLL.NewProc("NCryptFreeObject")

	// golang.org/x/sys/windows binds the whole of crypt32 that we need and none
	// of ncrypt, so every entry point above is resolved here. Resolved ONCE and
	// checked, because LazyProc.Addr panics on a missing export and a service
	// that panics on a twelve-hourly timer is worse than one that logs an
	// error.
	ncryptOnce sync.Once
	ncryptErr  error
)

func loadNCrypt() error {
	ncryptOnce.Do(func() {
		for _, p := range []*windows.LazyProc{
			procOpenStorageProvider, procCreatePersistedKey, procSetProperty,
			procFinalizeKey, procSignHash, procExportKey, procOpenKey,
			procDeleteKey, procFreeObject,
		} {
			if err := p.Find(); err != nil {
				ncryptErr = fmt.Errorf("netcert: ncrypt.dll: %w", err)
				return
			}
		}
	})
	return ncryptErr
}

// status turns a SECURITY_STATUS into an error naming the call that produced
// it. The raw value is kept in the message because FormatMessage renders these
// as sentences like "Object already exists" that do not say which object.
func status(op string, r uintptr) error {
	if r == statusSuccess {
		return nil
	}
	return fmt.Errorf("netcert: %s: %w (0x%08x)", op, syscall.Errno(r), uint32(r))
}

// cngSigner signs with a key that is inside a provider and cannot come out.
//
// It is a crypto.Signer and nothing more, which is what lets the CSR-building
// code stay exactly as it was: x509.CreateCertificateRequest asks for a public
// key and one signature over one digest, and never for the private half.
type cngSigner struct {
	provider windows.Handle
	key      windows.Handle
	pub      *ecdsa.PublicKey
	name     string

	closeOnce sync.Once
}

func (s *cngSigner) Public() crypto.PublicKey { return s.pub }

// Sign produces an ASN.1 DER ECDSA signature over digest.
//
// The rand argument is ignored, as it is for every hardware-backed signer: the
// provider has its own source and would not accept ours.
//
// The re-encoding at the bottom is the part that is easy to miss. NCrypt
// returns the signature as raw r || s, two fixed-width big-endian integers with
// nothing around them. Go's x509 layer expects the ASN.1 SEQUENCE of two
// INTEGERs. The two are the same numbers in different clothes, and handing back
// the raw pair produces a CSR that encodes without complaint, transmits without
// complaint, and verifies nowhere.
func (s *cngSigner) Sign(_ io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	if len(digest) == 0 {
		return nil, errors.New("netcert: nothing to sign")
	}
	raw, err := s.signHash(digest)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return nil, fmt.Errorf("netcert: signature is %d bytes, which is not an r||s pair", len(raw))
	}
	half := len(raw) / 2
	return asn1.Marshal(struct{ R, S *big.Int }{
		R: new(big.Int).SetBytes(raw[:half]),
		S: new(big.Int).SetBytes(raw[half:]),
	})
}

func (s *cngSigner) signHash(digest []byte) ([]byte, error) {
	// Two passes, which is the shape every NCrypt output call takes: ask with a
	// nil buffer to be told the size, then ask again with one that fits.
	var need uint32
	r, _, _ := syscall.SyscallN(procSignHash.Addr(),
		uintptr(s.key), 0,
		uintptr(unsafe.Pointer(&digest[0])), uintptr(len(digest)),
		0, 0,
		uintptr(unsafe.Pointer(&need)),
		ncryptSilentFlag,
	)
	if err := status("NCryptSignHash (size)", r); err != nil {
		return nil, err
	}
	sig := make([]byte, need)
	r, _, _ = syscall.SyscallN(procSignHash.Addr(),
		uintptr(s.key), 0,
		uintptr(unsafe.Pointer(&digest[0])), uintptr(len(digest)),
		uintptr(unsafe.Pointer(&sig[0])), uintptr(len(sig)),
		uintptr(unsafe.Pointer(&need)),
		ncryptSilentFlag,
	)
	if err := status("NCryptSignHash", r); err != nil {
		return nil, err
	}
	return sig[:need], nil
}

// Close releases the provider and key handles. The key itself stays in the
// store: that is what persisted means.
func (s *cngSigner) Close() {
	s.closeOnce.Do(func() {
		freeNCryptObject(s.key)
		freeNCryptObject(s.provider)
	})
}

// freeNCryptObject releases a provider or key handle. Zero is not a handle and
// is quietly ignored, because every caller is on a path where some earlier step
// may not have produced one.
func freeNCryptObject(h windows.Handle) {
	if h != 0 {
		syscall.SyscallN(procFreeObject.Addr(), uintptr(h))
	}
}

// openMachineKey opens an existing persisted container by name.
//
// The nteBadKeyset case is separated from real failures because "not there" is
// the ordinary answer for both callers: deletion, where the work is already
// done, and the tests, where it is the thing being asserted.
func openMachineKey(name string) (prov, key windows.Handle, found bool, err error) {
	if err := loadNCrypt(); err != nil {
		return 0, 0, false, err
	}
	provName, err := windows.UTF16PtrFromString(keyStorageProvider)
	if err != nil {
		return 0, 0, false, fmt.Errorf("netcert: provider name: %w", err)
	}
	r, _, _ := syscall.SyscallN(procOpenStorageProvider.Addr(),
		uintptr(unsafe.Pointer(&prov)), uintptr(unsafe.Pointer(provName)), 0)
	if err := status("NCryptOpenStorageProvider", r); err != nil {
		return 0, 0, false, err
	}

	keyName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		freeNCryptObject(prov)
		return 0, 0, false, fmt.Errorf("netcert: key name: %w", err)
	}
	r, _, _ = syscall.SyscallN(procOpenKey.Addr(),
		uintptr(prov), uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(keyName)), 0,
		ncryptMachineKeyFlag|ncryptSilentFlag,
	)
	runtime.KeepAlive(keyName)
	if r == nteBadKeyset {
		return prov, 0, false, nil
	}
	if err := status("NCryptOpenKey", r); err != nil {
		freeNCryptObject(prov)
		return 0, 0, false, err
	}
	return prov, key, true, nil
}

// createCNGKey makes a non-exportable P-256 key in the machine key store and
// returns a signer for it.
//
// The key is persisted before it has signed anything, which it has to be: a CNG
// key that is not finalized cannot sign, and finalizing writes it. That is the
// ordering that the file-based path deliberately avoids, and keyContainerName
// explains what is done about it.
func createCNGKey(name string) (*cngSigner, error) {
	if err := loadNCrypt(); err != nil {
		return nil, err
	}

	provName, err := windows.UTF16PtrFromString(keyStorageProvider)
	if err != nil {
		return nil, fmt.Errorf("netcert: provider name: %w", err)
	}
	var prov windows.Handle
	r, _, _ := syscall.SyscallN(procOpenStorageProvider.Addr(),
		uintptr(unsafe.Pointer(&prov)), uintptr(unsafe.Pointer(provName)), 0)
	if err := status("NCryptOpenStorageProvider", r); err != nil {
		return nil, err
	}

	s := &cngSigner{provider: prov, name: name}
	// Everything below can fail, and every one of those failures must not leak
	// the provider handle.
	defer func() {
		if s.pub == nil {
			s.Close()
		}
	}()

	algID, err := windows.UTF16PtrFromString(ecdsaP256Algorithm)
	if err != nil {
		return nil, fmt.Errorf("netcert: algorithm name: %w", err)
	}
	keyName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("netcert: key name: %w", err)
	}
	r, _, _ = syscall.SyscallN(procCreatePersistedKey.Addr(),
		uintptr(prov), uintptr(unsafe.Pointer(&s.key)),
		uintptr(unsafe.Pointer(algID)), uintptr(unsafe.Pointer(keyName)),
		0, // dwLegacyKeySpec: zero for a CNG key
		ncryptMachineKeyFlag|ncryptOverwriteKeyFlag,
	)
	if err := status("NCryptCreatePersistedKey", r); err != nil {
		return nil, err
	}

	// Zero export policy, stated rather than inherited. See the comment on the
	// constants: this is the property that decides whether the key is a secret
	// or a file.
	if err := s.setDWORD(exportPolicyProperty, 0); err != nil {
		return nil, err
	}
	if err := s.setDWORD(keyUsageProperty, allowSigning); err != nil {
		return nil, err
	}

	r, _, _ = syscall.SyscallN(procFinalizeKey.Addr(), uintptr(s.key), ncryptSilentFlag)
	if err := status("NCryptFinalizeKey", r); err != nil {
		return nil, err
	}

	pub, err := s.exportPublic()
	if err != nil {
		return nil, err
	}
	s.pub = pub
	return s, nil
}

func (s *cngSigner) setDWORD(property string, value uint32) error {
	name, err := windows.UTF16PtrFromString(property)
	if err != nil {
		return fmt.Errorf("netcert: property name: %w", err)
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	r, _, _ := syscall.SyscallN(procSetProperty.Addr(),
		uintptr(s.key), uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		ncryptSilentFlag,
	)
	return status("NCryptSetProperty "+property, r)
}

// exportPublic reads back the PUBLIC half, which is the only thing this key
// will ever hand out.
//
// The blob is a BCRYPT_ECCKEY_BLOB: a magic number, the size of one coordinate,
// then X and Y at that size, big-endian, with no leading-zero trimming.
func (s *cngSigner) exportPublic() (*ecdsa.PublicKey, error) {
	blobType, err := windows.UTF16PtrFromString(eccPublicBlob)
	if err != nil {
		return nil, fmt.Errorf("netcert: blob type: %w", err)
	}
	var need uint32
	r, _, _ := syscall.SyscallN(procExportKey.Addr(),
		uintptr(s.key), 0, uintptr(unsafe.Pointer(blobType)), 0,
		0, 0, uintptr(unsafe.Pointer(&need)), ncryptSilentFlag,
	)
	if err := status("NCryptExportKey (size)", r); err != nil {
		return nil, err
	}
	blob := make([]byte, need)
	r, _, _ = syscall.SyscallN(procExportKey.Addr(),
		uintptr(s.key), 0, uintptr(unsafe.Pointer(blobType)), 0,
		uintptr(unsafe.Pointer(&blob[0])), uintptr(len(blob)),
		uintptr(unsafe.Pointer(&need)), ncryptSilentFlag,
	)
	if err := status("NCryptExportKey", r); err != nil {
		return nil, err
	}
	blob = blob[:need]

	if len(blob) < 8 {
		return nil, fmt.Errorf("netcert: public blob is %d bytes", len(blob))
	}
	magic := binary.LittleEndian.Uint32(blob[0:4])
	if magic != ecdsaPublicP256Magic {
		return nil, fmt.Errorf("netcert: provider returned key type 0x%08x, not ECDSA P-256", magic)
	}
	size := int(binary.LittleEndian.Uint32(blob[4:8]))
	if size <= 0 || len(blob) < 8+2*size {
		return nil, fmt.Errorf("netcert: public blob claims %d-byte coordinates in %d bytes", size, len(blob))
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(blob[8 : 8+size]),
		Y:     new(big.Int).SetBytes(blob[8+size : 8+2*size]),
	}, nil
}

// deleteCNGKey removes a persisted container.
//
// A container that is not there is not an error: this runs on the failure path
// of a renewal that may itself have failed before the key was created, and on
// the sweep of a certificate whose key somebody has already removed by hand.
func deleteCNGKey(name string) error {
	prov, key, found, err := openMachineKey(name)
	if err != nil {
		return err
	}
	defer freeNCryptObject(prov)
	if !found {
		return nil
	}
	// NCryptDeleteKey frees the key handle itself on success, so there is no
	// freeNCryptObject to pair with the open above except on the failure path.
	r, _, _ := syscall.SyscallN(procDeleteKey.Addr(), uintptr(key), ncryptSilentFlag)
	if err := status("NCryptDeleteKey", r); err != nil {
		freeNCryptObject(key)
		return err
	}
	return nil
}
