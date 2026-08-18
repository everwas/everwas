// Package update implements the agent's signed self-update: download an
// artifact over HTTPS, check its SHA-256, verify a minisign signature with a
// key that shipped in the binary (or was rotated in at enrollment), then swap
// the running executable atomically with a rollback path if the new build
// turns out to be broken.
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// EmbeddedPublicKey is the release signing key, injected at build time with
// -ldflags "-X github.com/everwas/everwas/agent/internal/update.EmbeddedPublicKey=..."
// It holds the base64 body of a minisign public key (the second line of a
// .pub file), not the whole file.
//
// It is the trust anchor for the entire self-update path. A build without
// one cannot update, by design: see TrustedKeys.
var EmbeddedPublicKey = ""

// DevBuild opts a build out of the embedded-key requirement, for local
// development only. Set it with
// -ldflags "-X github.com/everwas/everwas/agent/internal/update.DevBuild=true".
//
// It exists so the escape hatch is EXPLICIT and greppable in a build
// command, rather than being the accidental consequence of leaving a key
// variable empty. A build with DevBuild=true will install an artifact signed
// by whatever key the server names, which is fine on a laptop and must never
// ship.
var DevBuild = ""

// Wire sizes of the minisign primitives.
const (
	algLen        = 2
	keyIDLen      = 8
	pubKeyBodyLen = algLen + keyIDLen + ed25519.PublicKeySize
	sigBodyLen    = algLen + keyIDLen + ed25519.SignatureSize
)

// algEdDSA is minisign's legacy (non-prehashed) algorithm: the ed25519
// signature covers the message bytes directly. algHashedEdDSA signs a
// BLAKE2b-512 digest instead, which needs a hash the standard library does
// not ship, so we reject it with an actionable error rather than pretending.
var (
	algEdDSA       = [algLen]byte{'E', 'd'}
	algHashedEdDSA = [algLen]byte{'E', 'D'}
)

// Verification failures. Callers branch on these to decide whether a failure
// is worth retrying (transport) or is a hard stop (anything below).
var (
	ErrMalformedKey       = errors.New("update: malformed public key")
	ErrMalformedSignature = errors.New("update: malformed signature")
	ErrUnsupportedAlg     = errors.New("update: unsupported signature algorithm")
	ErrPrehashedSignature = errors.New("update: prehashed minisign signature (sign without -H)")
	ErrKeyIDMismatch      = errors.New("update: signature key id does not match public key")
	ErrBadSignature       = errors.New("update: signature verification failed")
	ErrChecksumMismatch   = errors.New("update: sha256 checksum mismatch")
	ErrNoPublicKey        = errors.New("update: no signing public key available")
	ErrNoTrustAnchor      = errors.New("update: this build has no embedded signing key, so it cannot verify an update")
)

// PublicKey is a parsed minisign public key.
type PublicKey struct {
	Algorithm [algLen]byte
	KeyID     [keyIDLen]byte
	Key       ed25519.PublicKey
}

// KeyIDHex renders the key id the way minisign prints it, most significant
// byte first, for log lines and error messages.
func (p PublicKey) KeyIDHex() string { return keyIDHex(p.KeyID) }

// Signature is a parsed minisign signature file.
type Signature struct {
	Algorithm      [algLen]byte
	KeyID          [keyIDLen]byte
	Sig            []byte
	TrustedComment string
	GlobalSig      []byte
}

// KeyIDHex renders the signing key id, most significant byte first.
func (s Signature) KeyIDHex() string { return keyIDHex(s.KeyID) }

func keyIDHex(id [keyIDLen]byte) string {
	rev := make([]byte, keyIDLen)
	for i := range id {
		rev[keyIDLen-1-i] = id[i]
	}
	return strings.ToUpper(hex.EncodeToString(rev))
}

// ParsePublicKey accepts either a whole minisign .pub file (comment line plus
// base64 line) or the bare base64 body on its own.
func ParsePublicKey(s string) (PublicKey, error) {
	body, err := lastNonEmptyLine(s)
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: %v", ErrMalformedKey, err)
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: base64: %v", ErrMalformedKey, err)
	}
	if len(raw) != pubKeyBodyLen {
		return PublicKey{}, fmt.Errorf("%w: got %d bytes, want %d", ErrMalformedKey, len(raw), pubKeyBodyLen)
	}
	var pk PublicKey
	copy(pk.Algorithm[:], raw[:algLen])
	copy(pk.KeyID[:], raw[algLen:algLen+keyIDLen])
	pk.Key = ed25519.PublicKey(append([]byte(nil), raw[algLen+keyIDLen:]...))

	switch pk.Algorithm {
	case algEdDSA, algHashedEdDSA:
		// Minisign public keys always carry "Ed"; some tooling writes "ED".
		// Either is fine here because the signature file decides the mode.
	default:
		return PublicKey{}, fmt.Errorf("%w: key algorithm %q", ErrUnsupportedAlg, string(pk.Algorithm[:]))
	}
	return pk, nil
}

// ParseSignature parses a minisign .minisig file. The trusted comment and its
// global signature are optional; when present they are verified too, so a
// tampered trusted comment (which is where release metadata lives) fails.
func ParseSignature(b []byte) (Signature, error) {
	lines := nonEmptyLines(string(b))
	if len(lines) < 2 {
		return Signature{}, fmt.Errorf("%w: want at least 2 lines, got %d", ErrMalformedSignature, len(lines))
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return Signature{}, fmt.Errorf("%w: base64: %v", ErrMalformedSignature, err)
	}
	if len(raw) != sigBodyLen {
		return Signature{}, fmt.Errorf("%w: got %d bytes, want %d", ErrMalformedSignature, len(raw), sigBodyLen)
	}
	var sig Signature
	copy(sig.Algorithm[:], raw[:algLen])
	copy(sig.KeyID[:], raw[algLen:algLen+keyIDLen])
	sig.Sig = append([]byte(nil), raw[algLen+keyIDLen:]...)

	for _, l := range lines[2:] {
		if rest, ok := strings.CutPrefix(l, "trusted comment:"); ok {
			sig.TrustedComment = strings.TrimSpace(rest)
			continue
		}
		if sig.TrustedComment != "" && sig.GlobalSig == nil {
			g, err := base64.StdEncoding.DecodeString(l)
			if err != nil {
				return Signature{}, fmt.Errorf("%w: global sig base64: %v", ErrMalformedSignature, err)
			}
			if len(g) != ed25519.SignatureSize {
				return Signature{}, fmt.Errorf("%w: global sig is %d bytes, want %d",
					ErrMalformedSignature, len(g), ed25519.SignatureSize)
			}
			sig.GlobalSig = g
		}
	}
	return sig, nil
}

// Verify checks that signature (the bytes of a .minisig file) is a valid
// minisign signature over message by pub.
func Verify(pub PublicKey, message, signature []byte) error {
	sig, err := ParseSignature(signature)
	if err != nil {
		return err
	}
	return VerifyParsed(pub, message, sig)
}

// VerifyParsed is Verify with the signature file already parsed.
func VerifyParsed(pub PublicKey, message []byte, sig Signature) error {
	switch sig.Algorithm {
	case algEdDSA:
	case algHashedEdDSA:
		return fmt.Errorf("%w: key id %s", ErrPrehashedSignature, sig.KeyIDHex())
	default:
		return fmt.Errorf("%w: signature algorithm %q", ErrUnsupportedAlg, string(sig.Algorithm[:]))
	}
	if sig.KeyID != pub.KeyID {
		return fmt.Errorf("%w: signed by %s, trusted key is %s", ErrKeyIDMismatch, sig.KeyIDHex(), pub.KeyIDHex())
	}
	if len(pub.Key) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: key is %d bytes", ErrMalformedKey, len(pub.Key))
	}
	if !ed25519.Verify(pub.Key, message, sig.Sig) {
		return ErrBadSignature
	}
	if sig.GlobalSig != nil {
		global := append(append([]byte(nil), sig.Sig...), []byte(sig.TrustedComment)...)
		if !ed25519.Verify(pub.Key, global, sig.GlobalSig) {
			return fmt.Errorf("%w: trusted comment", ErrBadSignature)
		}
	}
	return nil
}

// VerifyAny accepts the signature if any trusted key validates it. The last
// error is returned when none do, and a key id mismatch is reported in
// preference to a parse error so the message points at the real problem.
func VerifyAny(pubs []PublicKey, message, signature []byte) error {
	if len(pubs) == 0 {
		return ErrNoPublicKey
	}
	var last error
	for _, pub := range pubs {
		err := Verify(pub, message, signature)
		if err == nil {
			return nil
		}
		if last == nil || errors.Is(last, ErrKeyIDMismatch) {
			last = err
		}
	}
	return last
}

// TrustedKeys parses the embedded key plus any rotated keys handed to the
// agent at enrollment. Duplicate key ids collapse to one.
//
// It FAILS when EmbeddedPublicKey is empty, and that is the whole point.
// The rotated keys in extra arrive from the server, and the invariant the
// update path is built on is that they are trusted IN ADDITION TO the
// embedded key, never instead of it, so a compromised server cannot name its
// own signing key. That invariant is vacuous when there is no embedded key:
// skipping the blank entry and carrying on left a build that would trust
// whatever the server supplied, which is a worse position than having no
// update mechanism at all.
//
// A build that genuinely wants no anchor has to say so, with
// -ldflags "-X ...update.DevBuild=true".
func TrustedKeys(extra ...string) ([]PublicKey, error) {
	if strings.TrimSpace(EmbeddedPublicKey) == "" && strings.TrimSpace(DevBuild) != "true" {
		return nil, ErrNoTrustAnchor
	}
	seen := make(map[[keyIDLen]byte]bool)
	var keys []PublicKey
	for _, s := range append([]string{EmbeddedPublicKey}, extra...) {
		if strings.TrimSpace(s) == "" {
			continue
		}
		pk, err := ParsePublicKey(s)
		if err != nil {
			return nil, err
		}
		if seen[pk.KeyID] {
			continue
		}
		seen[pk.KeyID] = true
		keys = append(keys, pk)
	}
	if len(keys) == 0 {
		return nil, ErrNoPublicKey
	}
	return keys, nil
}

// VerifySHA256 compares the hex digest of data against want (case
// insensitive). An empty want is a caller bug, not a pass.
func VerifySHA256(data []byte, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return fmt.Errorf("%w: no expected digest given", ErrChecksumMismatch)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, got, want)
	}
	return nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(strings.TrimSpace(l), "\r")
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func lastNonEmptyLine(s string) (string, error) {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return "", errors.New("empty")
	}
	return lines[len(lines)-1], nil
}
