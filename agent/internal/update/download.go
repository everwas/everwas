package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultMaxBytes caps an artifact at 256 MiB. The agent binary is a few tens
// of megabytes; the cap exists so a hostile or broken server cannot fill the
// disk before the signature ever gets checked.
const DefaultMaxBytes int64 = 256 << 20

// Download failures.
var (
	ErrDownload    = errors.New("update: download failed")
	ErrTooLarge    = errors.New("update: artifact exceeds size cap")
	ErrNotHTTPS    = errors.New("update: artifact URL must be https")
	ErrBadStatus   = errors.New("update: unexpected HTTP status")
	ErrEmptyResult = errors.New("update: server returned no bytes")
)

// Downloader fetches release artifacts. The zero value works: it uses a
// default client whose TLS config is the standard library's, which means the
// host's system CA pool, the same trust the enrollment and NATS paths use.
type Downloader struct {
	Client   *http.Client
	MaxBytes int64
	// AllowInsecureHTTP relaxes the https-only rule. Tests set it; production
	// never should.
	AllowInsecureHTTP bool
}

func (d *Downloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (d *Downloader) maxBytes() int64 {
	if d.MaxBytes > 0 {
		return d.MaxBytes
	}
	return DefaultMaxBytes
}

// StagingDir is where in-flight and verified artifacts live.
func StagingDir(stateDir string) string { return filepath.Join(stateDir, "staging") }

// StagedPath names the artifact for a version, with the platform's executable
// extension so a Windows staged binary can actually be launched.
func StagedPath(stateDir, version string) string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return filepath.Join(StagingDir(stateDir), "openrmm-agent-"+sanitizeVersion(version)+ext)
}

// sanitizeVersion keeps a server-supplied version string from escaping the
// staging directory or producing an unopenable filename.
func sanitizeVersion(v string) string {
	if v == "" {
		return "unknown"
	}
	keep := func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return r
		case r == '.', r == '-', r == '_', r == '+':
			return r
		default:
			return '_'
		}
	}
	s := strings.Map(keep, v)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// Download fetches url into dest, resuming a previous partial transfer when
// one is present. It writes to dest+".part" and renames on success, so dest
// only ever exists as a complete file. The returned size is the artifact size
// in bytes.
func (d *Downloader) Download(ctx context.Context, url, dest string) (int64, error) {
	if !d.AllowInsecureHTTP && !strings.HasPrefix(strings.ToLower(url), "https://") {
		return 0, fmt.Errorf("%w: %s", ErrNotHTTPS, url)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, fmt.Errorf("%w: staging dir: %v", ErrDownload, err)
	}

	part := dest + ".part"
	var have int64
	if fi, err := os.Stat(part); err == nil && fi.Mode().IsRegular() {
		have = fi.Size()
	}
	if have > d.maxBytes() {
		// A stale part file larger than the cap is junk; start over.
		_ = os.Remove(part)
		have = 0
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	req.Header.Set("User-Agent", "openrmm-agent-updater")
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := d.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer resp.Body.Close()

	appending := false
	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored our Range header (or we had nothing to resume).
		// Restart from zero rather than splicing mismatched byte ranges.
	case http.StatusPartialContent:
		appending = have > 0
	case http.StatusRequestedRangeNotSatisfiable:
		// The part file is at or past the artifact length: it is stale.
		_ = os.Remove(part)
		return 0, fmt.Errorf("%w: stale partial download discarded, retry", ErrDownload)
	default:
		return 0, fmt.Errorf("%w: %s for %s", ErrBadStatus, resp.Status, url)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if appending {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
		have = 0
	}
	f, err := os.OpenFile(part, flags, 0o700)
	if err != nil {
		return 0, fmt.Errorf("%w: open staging file: %v", ErrDownload, err)
	}

	remaining := d.maxBytes() - have
	// Read one byte past the cap so an oversized body is detected instead of
	// being silently truncated into a checksum failure.
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, remaining+1))
	closeErr := f.Close()
	if copyErr != nil {
		// Leave the part file in place: the next attempt resumes from here.
		return 0, fmt.Errorf("%w: %v", ErrDownload, copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("%w: close staging file: %v", ErrDownload, closeErr)
	}
	total := have + written
	if written > remaining {
		_ = os.Remove(part)
		return 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, d.maxBytes())
	}
	if total == 0 {
		_ = os.Remove(part)
		return 0, ErrEmptyResult
	}
	if err := os.Rename(part, dest); err != nil {
		return 0, fmt.Errorf("%w: stage artifact: %v", ErrDownload, err)
	}
	if err := os.Chmod(dest, 0o700); err != nil {
		return 0, fmt.Errorf("%w: chmod staged artifact: %v", ErrDownload, err)
	}
	return total, nil
}

// Fetch retrieves a small companion file (a signature or a SHA256SUMS line)
// into memory. maxBytes guards against a server streaming forever.
func (d *Downloader) Fetch(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	if !d.AllowInsecureHTTP && !strings.HasPrefix(strings.ToLower(url), "https://") {
		return nil, fmt.Errorf("%w: %s", ErrNotHTTPS, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	req.Header.Set("User-Agent", "openrmm-agent-updater")
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s for %s", ErrBadStatus, resp.Status, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w: companion file over %d bytes", ErrTooLarge, maxBytes)
	}
	return body, nil
}

// CleanStaging removes staged artifacts other than keep. It is best effort:
// a failure to tidy up is never a reason to fail an update.
func CleanStaging(stateDir string, keep ...string) {
	dir := StagingDir(stateDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		if k != "" {
			kept[filepath.Base(k)] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() || kept[e.Name()] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
