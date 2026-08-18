package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestInteropWithRealMinisign is the only check that proves our stdlib
// verifier agrees with the actual minisign tool rather than with our own idea
// of the format. It skips when minisign is not installed, so it is free
// locally and meaningful in CI, where minisign is present because releases
// are signed with it.
//
// Everything happens in a temp dir with a throwaway key. It never touches the
// real release key and never reaches the network.
func TestInteropWithRealMinisign(t *testing.T) {
	bin, err := exec.LookPath("minisign")
	if err != nil {
		t.Skip("minisign not installed; install it to run the interop check")
	}

	dir := t.TempDir()
	secKey := filepath.Join(dir, "test.key")
	pubKey := filepath.Join(dir, "test.pub")
	artifact := filepath.Join(dir, "artifact.bin")
	sigFile := artifact + ".minisig"

	if err := os.WriteFile(artifact, []byte("pretend this is an agent binary"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	// -W generates an unencrypted key so signing needs no passphrase prompt.
	run(t, bin, "-G", "-W", "-p", pubKey, "-s", secKey)
	// No -H: the agent verifies the raw artifact, not a BLAKE2b prehash.
	run(t, bin, "-S", "-s", secKey, "-m", artifact, "-x", sigFile, "-t", "everwas interop test")

	pubBytes, err := os.ReadFile(pubKey)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	pub, err := ParsePublicKey(string(pubBytes))
	if err != nil {
		t.Fatalf("ParsePublicKey on a real minisign key: %v", err)
	}

	sig, err := os.ReadFile(sigFile)
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	body, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if err := Verify(pub, body, sig); err != nil {
		t.Fatalf("Verify on a real minisign signature: %v", err)
	}

	body[0] ^= 0xff
	if err := Verify(pub, body, sig); err == nil {
		t.Fatal("a tampered artifact verified against a real minisign signature")
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
