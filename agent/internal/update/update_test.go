package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// artifactServer serves a fixed body from a local httptest listener. Nothing
// leaves the machine, and no real release is ever fetched by the tests.
func artifactServer(t *testing.T, body []byte, sig []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".minisig"):
			_, _ = w.Write(sig)
		case r.URL.Path == "/agent":
			http.ServeContent(w, r, "agent", time.Time{}, strings.NewReader(string(body)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestApplyHappyPath(t *testing.T) {
	k := newTestKey(t, 0x01)
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = k.pub

	body := []byte("brand new agent binary")
	srv := artifactServer(t, body, k.sign(body, "timestamp:1700000000", algEdDSA))

	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")

	res, err := Apply(context.Background(), Request{
		Version:      "2.0.0",
		ArtifactURL:  srv.URL + "/agent",
		SHA256:       sha256hex(body),
		SignatureURL: srv.URL + "/agent.minisig",
	}, Options{
		StateDir:       stateDir,
		TargetPath:     target,
		CurrentVersion: "1.0.0",
		Downloader:     &Downloader{AllowInsecureHTTP: true},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, target); got != string(body) {
		t.Errorf("target = %q, want the downloaded artifact", got)
	}
	if got := readFile(t, res.Backup); got != "old build" {
		t.Errorf("backup = %q, want the previous build", got)
	}

	st, err := NewTracker(stateDir).Load()
	if err != nil {
		t.Fatalf("Load state: %v", err)
	}
	if !st.Pending() || st.PendingVersion != "2.0.0" {
		t.Errorf("state = %+v, want 2.0.0 pending", st)
	}
	// Staging is tidied so a half finished artifact cannot be reused later.
	entries, err := os.ReadDir(StagingDir(stateDir))
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staging dir still holds %d entries", len(entries))
	}
}

func TestApplyRejectsChecksumMismatch(t *testing.T) {
	k := newTestKey(t, 0x02)
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = k.pub

	body := []byte("brand new agent binary")
	srv := artifactServer(t, body, k.sign(body, "", algEdDSA))

	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")

	_, err := Apply(context.Background(), Request{
		Version:      "2.0.0",
		ArtifactURL:  srv.URL + "/agent",
		SHA256:       sha256hex([]byte("something else entirely")),
		SignatureURL: srv.URL + "/agent.minisig",
	}, Options{
		StateDir:       stateDir,
		TargetPath:     target,
		CurrentVersion: "1.0.0",
		Downloader:     &Downloader{AllowInsecureHTTP: true},
	})
	assertStep(t, err, StepChecksum, ErrChecksumMismatch)
	assertUntouched(t, target, stateDir)
}

func TestApplyRejectsBadSignature(t *testing.T) {
	signer := newTestKey(t, 0x03)
	trusted := newTestKey(t, 0x04)
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = trusted.pub

	body := []byte("attacker supplied binary")
	srv := artifactServer(t, body, signer.sign(body, "", algEdDSA))

	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")

	_, err := Apply(context.Background(), Request{
		Version:      "2.0.0",
		ArtifactURL:  srv.URL + "/agent",
		SHA256:       sha256hex(body),
		SignatureURL: srv.URL + "/agent.minisig",
	}, Options{
		StateDir:       stateDir,
		TargetPath:     target,
		CurrentVersion: "1.0.0",
		Downloader:     &Downloader{AllowInsecureHTTP: true},
	})
	assertStep(t, err, StepSignature, ErrKeyIDMismatch)
	assertUntouched(t, target, stateDir)
}

func TestApplyAcceptsRotatedKeyFromEnrollment(t *testing.T) {
	embedded := newTestKey(t, 0x05)
	rotated := newTestKey(t, 0x06)
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = embedded.pub

	body := []byte("release signed with the rotated key")
	srv := artifactServer(t, body, rotated.sign(body, "", algEdDSA))

	stateDir := t.TempDir()
	binDir := t.TempDir()
	target := fakeBinary(t, binDir, "openrmm-agent", "old build")

	if _, err := Apply(context.Background(), Request{
		Version:      "2.0.0",
		ArtifactURL:  srv.URL + "/agent",
		SHA256:       sha256hex(body),
		SignatureURL: srv.URL + "/agent.minisig",
		PublicKeys:   []string{rotated.pub},
	}, Options{
		StateDir:       stateDir,
		TargetPath:     target,
		CurrentVersion: "1.0.0",
		Downloader:     &Downloader{AllowInsecureHTTP: true},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, target); got != string(body) {
		t.Errorf("target = %q, want the downloaded artifact", got)
	}
}

func TestApplyValidatesRequest(t *testing.T) {
	k := newTestKey(t, 0x07)
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = k.pub

	base := Request{Version: "2.0.0", ArtifactURL: "https://example.com/a", SHA256: "ab", SignatureURL: "https://example.com/a.minisig"}
	opts := Options{StateDir: t.TempDir(), TargetPath: filepath.Join(t.TempDir(), "agent"), CurrentVersion: "1.0.0"}

	cases := map[string]Request{
		"no version": {ArtifactURL: base.ArtifactURL, SHA256: base.SHA256, SignatureURL: base.SignatureURL},
		"no url":     {Version: base.Version, SHA256: base.SHA256, SignatureURL: base.SignatureURL},
		"no sha":     {Version: base.Version, ArtifactURL: base.ArtifactURL, SignatureURL: base.SignatureURL},
		"no sig":     {Version: base.Version, ArtifactURL: base.ArtifactURL, SHA256: base.SHA256},
	}
	for name, req := range cases {
		_, err := Apply(context.Background(), req, opts)
		var ue *Error
		if !errors.As(err, &ue) || ue.Step != StepValidate {
			t.Errorf("%s: err = %v, want a validate step error", name, err)
		}
	}

	same := base
	same.Version = "1.0.0"
	if _, err := Apply(context.Background(), same, opts); !errors.Is(err, ErrAlreadyCurrent) {
		t.Errorf("err = %v, want ErrAlreadyCurrent", err)
	}
}

func TestApplyRequiresATrustAnchor(t *testing.T) {
	orig := EmbeddedPublicKey
	t.Cleanup(func() { EmbeddedPublicKey = orig })
	EmbeddedPublicKey = ""

	_, err := Apply(context.Background(), Request{
		Version:      "2.0.0",
		ArtifactURL:  "https://example.invalid/agent",
		SHA256:       "ab",
		SignatureURL: "https://example.invalid/agent.minisig",
	}, Options{StateDir: t.TempDir(), TargetPath: filepath.Join(t.TempDir(), "agent"), CurrentVersion: "1.0.0"})
	assertStep(t, err, StepKeys, ErrNoPublicKey)
}

func assertStep(t *testing.T, err error, want Step, wantErr error) {
	t.Helper()
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *update.Error", err)
	}
	if ue.Step != want {
		t.Errorf("step = %s, want %s", ue.Step, want)
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
}

// assertUntouched checks the failure was total: the binary is unchanged, no
// backup was made, no update is pending, and the staged artifact is gone.
func assertUntouched(t *testing.T, target, stateDir string) {
	t.Helper()
	if got := readFile(t, target); got != "old build" {
		t.Errorf("target = %q, want it untouched", got)
	}
	if HasBackup(target) {
		t.Error("a rejected artifact must not produce a backup")
	}
	st, err := NewTracker(stateDir).Load()
	if err != nil {
		t.Fatalf("Load state: %v", err)
	}
	if st.Pending() {
		t.Errorf("state = %+v, want no update pending", st)
	}
	entries, err := os.ReadDir(StagingDir(stateDir))
	if err == nil && len(entries) != 0 {
		t.Errorf("staging dir still holds %d entries", len(entries))
	}
}
