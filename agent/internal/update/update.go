package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// Step names the phase an update failed in. It is carried on *Error so the
// server can tell a flaky network apart from a bad signature.
type Step string

const (
	StepValidate  Step = "validate"
	StepKeys      Step = "keys"
	StepDownload  Step = "download"
	StepChecksum  Step = "checksum"
	StepSignature Step = "signature"
	StepStage     Step = "stage"
	StepSwap      Step = "swap"
)

// Error is a failure attributed to one step of the update.
type Error struct {
	Step Step
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("update %s: %v", e.Step, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func stepErr(step Step, err error) error { return &Error{Step: step, Err: err} }

// ErrAlreadyCurrent means the requested version is the one already running.
var ErrAlreadyCurrent = errors.New("update: already running the requested version")

// Request is an update instruction, normally delivered over NATS.
type Request struct {
	Version     string `json:"version"`
	ArtifactURL string `json:"artifact_url"`
	SHA256      string `json:"sha256"`
	// Signature is the contents of the .minisig file. When empty,
	// SignatureURL is fetched instead.
	Signature    []byte `json:"signature,omitempty"`
	SignatureURL string `json:"signature_url,omitempty"`
	// PublicKeys are rotated signing keys delivered at enrollment. They are
	// trusted in addition to EmbeddedPublicKey, never instead of it, so a
	// server compromise cannot swap the trust anchor on its own.
	PublicKeys []string `json:"public_keys,omitempty"`
}

// Options are the local knobs Apply needs.
type Options struct {
	StateDir       string
	TargetPath     string // defaults to the running executable
	CurrentVersion string
	Downloader     *Downloader
	Log            *slog.Logger
}

// Result describes a completed update. The process must exit after a
// successful Apply so the service manager starts the new binary.
type Result struct {
	Version    string
	Target     string
	Backup     string
	StagedPath string
	Bytes      int64
	// Finalizing is true when the swap was handed to an external helper
	// process (the Windows fallback) and completes after this process exits.
	Finalizing bool
}

const maxSignatureBytes = 8 << 10

// Apply runs the whole pipeline: download, checksum, signature, swap. It
// never swaps a binary that failed any check, and it leaves the previous
// binary on disk for the rollback path.
func Apply(ctx context.Context, req Request, opts Options) (*Result, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	if req.Version == "" {
		return nil, stepErr(StepValidate, errors.New("no version given"))
	}
	if req.ArtifactURL == "" {
		return nil, stepErr(StepValidate, errors.New("no artifact URL given"))
	}
	if req.SHA256 == "" {
		return nil, stepErr(StepValidate, errors.New("no sha256 given"))
	}
	if len(req.Signature) == 0 && req.SignatureURL == "" {
		return nil, stepErr(StepValidate, errors.New("no signature given"))
	}
	if opts.StateDir == "" {
		return nil, stepErr(StepValidate, errors.New("no state dir given"))
	}
	if opts.CurrentVersion != "" && req.Version == opts.CurrentVersion {
		return nil, ErrAlreadyCurrent
	}

	target := opts.TargetPath
	if target == "" {
		exe, err := ExecutablePath()
		if err != nil {
			return nil, stepErr(StepValidate, fmt.Errorf("resolve running binary: %w", err))
		}
		target = exe
	}

	keys, err := TrustedKeys(req.PublicKeys...)
	if err != nil {
		return nil, stepErr(StepKeys, err)
	}

	dl := opts.Downloader
	if dl == nil {
		dl = &Downloader{}
	}

	staged := StagedPath(opts.StateDir, req.Version)
	log.Info("update starting", "version", req.Version, "url", req.ArtifactURL, "staged", staged)

	n, err := dl.Download(ctx, req.ArtifactURL, staged)
	if err != nil {
		return nil, stepErr(StepDownload, err)
	}

	body, err := os.ReadFile(staged)
	if err != nil {
		discard(staged)
		return nil, stepErr(StepStage, fmt.Errorf("read staged artifact: %w", err))
	}

	if err := VerifySHA256(body, req.SHA256); err != nil {
		discard(staged)
		return nil, stepErr(StepChecksum, err)
	}

	sig := req.Signature
	if len(sig) == 0 {
		sig, err = dl.Fetch(ctx, req.SignatureURL, maxSignatureBytes)
		if err != nil {
			discard(staged)
			return nil, stepErr(StepSignature, err)
		}
	}
	if err := VerifyAny(keys, body, sig); err != nil {
		// A bad signature means the artifact is not ours. Remove it so a
		// later bug cannot pick it up off the staging directory.
		discard(staged)
		return nil, stepErr(StepSignature, err)
	}
	log.Info("update artifact verified", "version", req.Version, "bytes", n)

	tracker := NewTracker(opts.StateDir)
	backup := BackupPath(target)
	if err := tracker.BeginUpdate(req.Version, opts.CurrentVersion, target, backup); err != nil {
		discard(staged)
		return nil, stepErr(StepStage, err)
	}

	res, swapErr := Swap(target, staged)
	if swapErr != nil {
		if NeedsFinalizer() {
			log.Warn("in-place swap refused, handing off to finalizer", "err", swapErr)
			if fErr := SpawnFinalizer(staged, target); fErr == nil {
				return &Result{
					Version:    req.Version,
					Target:     target,
					Backup:     backup,
					StagedPath: staged,
					Bytes:      n,
					Finalizing: true,
				}, nil
			}
		}
		// No swap happened, so there is nothing on probation.
		_ = tracker.Clear()
		discard(staged)
		return nil, stepErr(StepSwap, swapErr)
	}

	CleanStaging(opts.StateDir)
	log.Info("update applied, exiting for restart", "version", req.Version, "backup", res.Backup)
	return &Result{
		Version:    req.Version,
		Target:     res.Target,
		Backup:     res.Backup,
		StagedPath: staged,
		Bytes:      n,
	}, nil
}

func discard(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".part")
}
