package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// testKey is a minisign keypair generated inside the test. Nothing here
// touches the network or the real signing key.
type testKey struct {
	pub    string
	priv   ed25519.PrivateKey
	keyID  [keyIDLen]byte
	parsed PublicKey
}

func newTestKey(t *testing.T, id byte) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var keyID [keyIDLen]byte
	for i := range keyID {
		keyID[i] = id + byte(i)
	}
	body := append([]byte{'E', 'd'}, keyID[:]...)
	body = append(body, pub...)
	encoded := base64.StdEncoding.EncodeToString(body)
	parsed, err := ParsePublicKey(encoded)
	if err != nil {
		t.Fatalf("parse generated key: %v", err)
	}
	return testKey{pub: encoded, priv: priv, keyID: keyID, parsed: parsed}
}

// sign builds a minisign signature file the way the minisign tool would.
func (k testKey) sign(msg []byte, trustedComment string, alg [2]byte) []byte {
	sig := ed25519.Sign(k.priv, msg)
	body := append(append([]byte{alg[0], alg[1]}, k.keyID[:]...), sig...)

	var b strings.Builder
	b.WriteString("untrusted comment: signature from minisign secret key\n")
	b.WriteString(base64.StdEncoding.EncodeToString(body))
	b.WriteString("\n")
	if trustedComment != "" {
		global := ed25519.Sign(k.priv, append(append([]byte(nil), sig...), []byte(trustedComment)...))
		b.WriteString("trusted comment: " + trustedComment + "\n")
		b.WriteString(base64.StdEncoding.EncodeToString(global))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func TestParsePublicKeyFormats(t *testing.T) {
	k := newTestKey(t, 0x11)
	file := "untrusted comment: minisign public key ABCDEF\n" + k.pub + "\n"

	for name, in := range map[string]string{"bare": k.pub, "file": file} {
		got, err := ParsePublicKey(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.KeyID != k.keyID {
			t.Errorf("%s: key id = %x, want %x", name, got.KeyID, k.keyID)
		}
		if !got.Key.Equal(k.parsed.Key) {
			t.Errorf("%s: public key bytes differ", name)
		}
	}
}

func TestParsePublicKeyRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"not b64":   "not base64 at all!!",
		"too short": base64.StdEncoding.EncodeToString([]byte("Ed12345")),
		"bad alg":   base64.StdEncoding.EncodeToString(append([]byte("XX01234567"), make([]byte, 32)...)),
	}
	for name, in := range cases {
		if _, err := ParsePublicKey(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestVerifyValidSignature(t *testing.T) {
	k := newTestKey(t, 0x20)
	msg := []byte("everwas-agent binary contents")
	if err := Verify(k.parsed, msg, k.sign(msg, "timestamp:1700000000 file:everwas-agent", algEdDSA)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyWithoutTrustedComment(t *testing.T) {
	k := newTestKey(t, 0x21)
	msg := []byte("payload")
	if err := Verify(k.parsed, msg, k.sign(msg, "", algEdDSA)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyTamperedMessage(t *testing.T) {
	k := newTestKey(t, 0x30)
	msg := []byte("everwas-agent binary contents")
	sig := k.sign(msg, "timestamp:1700000000", algEdDSA)

	tampered := append([]byte(nil), msg...)
	tampered[0] ^= 0xff
	err := Verify(k.parsed, tampered, sig)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyTamperedTrustedComment(t *testing.T) {
	k := newTestKey(t, 0x31)
	msg := []byte("payload")
	sig := string(k.sign(msg, "timestamp:1700000000", algEdDSA))
	sig = strings.Replace(sig, "timestamp:1700000000", "timestamp:1900000000", 1)

	err := Verify(k.parsed, msg, []byte(sig))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyWrongKeyID(t *testing.T) {
	signer := newTestKey(t, 0x40)
	trusted := newTestKey(t, 0x50)
	msg := []byte("payload")

	err := Verify(trusted.parsed, msg, signer.sign(msg, "", algEdDSA))
	if !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("err = %v, want ErrKeyIDMismatch", err)
	}
}

// A key id that matches while the key itself does not is the interesting
// forgery case: the id is attacker controlled metadata, the signature is not.
func TestVerifyMatchingKeyIDWrongKey(t *testing.T) {
	signer := newTestKey(t, 0x60)
	other := newTestKey(t, 0x60)
	msg := []byte("payload")

	err := Verify(other.parsed, msg, signer.sign(msg, "", algEdDSA))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyPrehashedRejected(t *testing.T) {
	k := newTestKey(t, 0x70)
	msg := []byte("payload")

	err := Verify(k.parsed, msg, k.sign(msg, "", algHashedEdDSA))
	if !errors.Is(err, ErrPrehashedSignature) {
		t.Fatalf("err = %v, want ErrPrehashedSignature", err)
	}
}

func TestVerifyMalformedSignature(t *testing.T) {
	k := newTestKey(t, 0x80)
	cases := map[string][]byte{
		"empty":        []byte(""),
		"one line":     []byte("untrusted comment: only\n"),
		"not base64":   []byte("untrusted comment: x\n@@@not base64@@@\n"),
		"short body":   []byte("untrusted comment: x\n" + base64.StdEncoding.EncodeToString([]byte("Ed0123")) + "\n"),
		"bad global":   []byte("untrusted comment: x\n" + base64.StdEncoding.EncodeToString(append([]byte("Ed01234567"), make([]byte, 64)...)) + "\ntrusted comment: t\n!!!\n"),
		"short global": []byte("untrusted comment: x\n" + base64.StdEncoding.EncodeToString(append([]byte("Ed01234567"), make([]byte, 64)...)) + "\ntrusted comment: t\n" + base64.StdEncoding.EncodeToString([]byte("short")) + "\n"),
	}
	for name, sig := range cases {
		err := Verify(k.parsed, []byte("payload"), sig)
		if !errors.Is(err, ErrMalformedSignature) {
			t.Errorf("%s: err = %v, want ErrMalformedSignature", name, err)
		}
	}
}

func TestVerifyAnyAcceptsRotatedKey(t *testing.T) {
	embedded := newTestKey(t, 0x90)
	rotated := newTestKey(t, 0xa0)
	msg := []byte("payload")
	sig := rotated.sign(msg, "", algEdDSA)

	if err := VerifyAny([]PublicKey{embedded.parsed, rotated.parsed}, msg, sig); err != nil {
		t.Fatalf("VerifyAny: %v", err)
	}
	if err := VerifyAny([]PublicKey{embedded.parsed}, msg, sig); !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("err = %v, want ErrKeyIDMismatch", err)
	}
	if err := VerifyAny(nil, msg, sig); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("err = %v, want ErrNoPublicKey", err)
	}
}

func TestTrustedKeys(t *testing.T) {
	embedded := newTestKey(t, 0xb0)
	rotated := newTestKey(t, 0xc0)

	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })

	EmbeddedPublicKey = embedded.pub
	keys, err := TrustedKeys(rotated.pub, "", rotated.pub)
	if err != nil {
		t.Fatalf("TrustedKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2 (duplicates collapse)", len(keys))
	}

	if _, err := TrustedKeys("nonsense"); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("err = %v, want ErrMalformedKey", err)
	}
}

// TestTrustedKeysRefusesAnEmptyEmbeddedKey is the regression for the defect
// that handed the update trust anchor to the server.
//
// The documented invariant is that rotated keys are trusted IN ADDITION TO
// the embedded key, never instead of it, so a compromised server cannot swap
// the anchor. Skipping a blank embedded key silently inverted that: a build
// with EmbeddedPublicKey="" trusted whatever key the server put in
// PublicKeys, and every artifact it signed verified. The goreleaser header
// told operators to build exactly that build for snapshots, claiming
// self-update would refuse to run.
func TestTrustedKeysRefusesAnEmptyEmbeddedKey(t *testing.T) {
	attacker := newTestKey(t, 0xd0)

	origKey, origDev := EmbeddedPublicKey, DevBuild
	t.Cleanup(func() { EmbeddedPublicKey, DevBuild = origKey, origDev })
	EmbeddedPublicKey, DevBuild = "", ""

	if _, err := TrustedKeys(); !errors.Is(err, ErrNoTrustAnchor) {
		t.Fatalf("TrustedKeys() = %v, want ErrNoTrustAnchor", err)
	}
	// The one that matters: a server-supplied key must not become the anchor.
	keys, err := TrustedKeys(attacker.pub)
	if !errors.Is(err, ErrNoTrustAnchor) {
		t.Fatalf("TrustedKeys(server key) = %v, want ErrNoTrustAnchor", err)
	}
	if len(keys) != 0 {
		t.Fatalf("TrustedKeys returned %d keys for a build with no anchor", len(keys))
	}
	// Whitespace is not a key either.
	EmbeddedPublicKey = "   \n\t "
	if _, err := TrustedKeys(attacker.pub); !errors.Is(err, ErrNoTrustAnchor) {
		t.Fatalf("TrustedKeys with a whitespace anchor = %v, want ErrNoTrustAnchor", err)
	}
}

// TestTrustedKeysDevBuildEscapeHatch keeps unsigned local builds workable,
// but only when the build said so out loud.
func TestTrustedKeysDevBuildEscapeHatch(t *testing.T) {
	local := newTestKey(t, 0xe0)

	origKey, origDev := EmbeddedPublicKey, DevBuild
	t.Cleanup(func() { EmbeddedPublicKey, DevBuild = origKey, origDev })
	EmbeddedPublicKey, DevBuild = "", "true"

	keys, err := TrustedKeys(local.pub)
	if err != nil {
		t.Fatalf("TrustedKeys in a dev build: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	// A dev build with nothing at all still has nothing to verify against.
	if _, err := TrustedKeys(); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("err = %v, want ErrNoPublicKey", err)
	}
	// Anything other than the exact opt-in is not an opt-in.
	for _, flag := range []string{"1", "yes", "TRUE", "false", ""} {
		DevBuild = flag
		if _, err := TrustedKeys(local.pub); !errors.Is(err, ErrNoTrustAnchor) {
			t.Errorf("DevBuild=%q opted out of the trust anchor", flag)
		}
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("hello")
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	if err := VerifySHA256(data, want); err != nil {
		t.Fatalf("VerifySHA256: %v", err)
	}
	if err := VerifySHA256(data, strings.ToUpper(want)); err != nil {
		t.Fatalf("uppercase digest should verify: %v", err)
	}
	if err := VerifySHA256([]byte("hello!"), want); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if err := VerifySHA256(data, ""); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("empty digest must not pass: %v", err)
	}
}

func TestKeyIDHex(t *testing.T) {
	pk, err := ParsePublicKey(base64.StdEncoding.EncodeToString(
		append(append([]byte("Ed"), []byte{1, 2, 3, 4, 5, 6, 7, 8}...), make([]byte, 32)...)))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if got := pk.KeyIDHex(); got != "0807060504030201" {
		t.Errorf("KeyIDHex = %s, want 0807060504030201", got)
	}
}
