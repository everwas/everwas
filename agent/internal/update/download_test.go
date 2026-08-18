package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadWritesCompleteFile(t *testing.T) {
	body := strings.Repeat("agent bytes ", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "agent", time.Time{}, strings.NewReader(body))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "staging", "agent")
	d := &Downloader{AllowInsecureHTTP: true}
	n, err := d.Download(context.Background(), srv.URL, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("wrote %d bytes, want %d", n, len(body))
	}
	if got := readFile(t, dest); got != body {
		t.Error("downloaded contents differ from the served body")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Errorf("part file left behind, stat err = %v", err)
	}
}

func TestDownloadResumesPartialFile(t *testing.T) {
	body := strings.Repeat("0123456789", 500)
	var sawRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		http.ServeContent(w, r, "agent", time.Time{}, strings.NewReader(body))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "agent")
	if err := os.WriteFile(dest+".part", []byte(body[:1000]), 0o700); err != nil {
		t.Fatalf("seed part file: %v", err)
	}

	d := &Downloader{AllowInsecureHTTP: true}
	n, err := d.Download(context.Background(), srv.URL, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if sawRange != "bytes=1000-" {
		t.Errorf("Range header = %q, want bytes=1000-", sawRange)
	}
	if n != int64(len(body)) {
		t.Errorf("total = %d, want %d", n, len(body))
	}
	if got := readFile(t, dest); got != body {
		t.Error("resumed file does not match the served body")
	}
}

func TestDownloadEnforcesSizeCap(t *testing.T) {
	body := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "agent")
	d := &Downloader{AllowInsecureHTTP: true, MaxBytes: 1024}
	if _, err := d.Download(context.Background(), srv.URL, dest); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("oversized artifact should not be staged, stat err = %v", err)
	}
}

func TestDownloadRejectsPlainHTTP(t *testing.T) {
	d := &Downloader{}
	_, err := d.Download(context.Background(), "http://example.invalid/agent", filepath.Join(t.TempDir(), "agent"))
	if !errors.Is(err, ErrNotHTTPS) {
		t.Fatalf("err = %v, want ErrNotHTTPS", err)
	}
	if _, err := d.Fetch(context.Background(), "http://example.invalid/agent.minisig", 1024); !errors.Is(err, ErrNotHTTPS) {
		t.Fatalf("Fetch err = %v, want ErrNotHTTPS", err)
	}
}

func TestDownloadReportsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	d := &Downloader{AllowInsecureHTTP: true}
	if _, err := d.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "agent")); !errors.Is(err, ErrBadStatus) {
		t.Fatalf("err = %v, want ErrBadStatus", err)
	}
}

func TestDownloadHonoursContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &Downloader{AllowInsecureHTTP: true}
	if _, err := d.Download(ctx, srv.URL, filepath.Join(t.TempDir(), "agent")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestFetchCapsCompanionFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("s", 2048)))
	}))
	defer srv.Close()

	d := &Downloader{AllowInsecureHTTP: true}
	if _, err := d.Fetch(context.Background(), srv.URL, 64); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	body, err := d.Fetch(context.Background(), srv.URL, 4096)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(body) != 2048 {
		t.Errorf("got %d bytes, want 2048", len(body))
	}
}

func TestStagedPathSanitizesVersion(t *testing.T) {
	dir := StagingDir("/var/lib/everwas")
	for _, version := range []string{"../../etc/passwd", `..\..\windows\system32`, "2.0.0"} {
		got := StagedPath("/var/lib/everwas", version)
		if filepath.Dir(filepath.Clean(got)) != dir {
			t.Errorf("StagedPath(%q) = %s, want it directly under %s", version, got, dir)
		}
	}
	if StagedPath("/s", "") == "" {
		t.Error("an empty version should still produce a path")
	}
}

func TestCleanStagingKeepsNamedFile(t *testing.T) {
	stateDir := t.TempDir()
	dir := StagingDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keep := filepath.Join(dir, "keep-me")
	for _, name := range []string{"keep-me", "old-1", "old-2"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	CleanStaging(stateDir, keep)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep-me" {
		t.Errorf("staging holds %v, want only keep-me", entries)
	}
}
