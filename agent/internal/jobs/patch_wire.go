package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/inventory"
	"github.com/rsp2k/openrmm/agent/internal/patch"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
)

// Audit event names for patch work. These belong next to the others in
// internal/audit; they live here until that constant block is extended.
const (
	EventPatchScanned   = "patch.scanned"
	EventPatchInstalled = "patch.installed"
)

// exitBusy is the exit code reported when the OS update backend was busy.
// It is 75 (EX_TEMPFAIL) so the server can tell "try again in a bit" apart
// from "this update will never install".
const exitBusy = 75

// Default job timeouts. A patch scan is minutes (a Windows Update search
// alone runs 2 to 10); an install is hours on a machine that has not been
// patched in a year. Both are overridden by the job's own timeout_s.
const (
	defaultScanTimeout    = 30 * time.Minute
	defaultInstallTimeout = 4 * time.Hour
)

// PatchDeps is everything the patch job handlers need, injected rather than
// reached for, so the handlers can be tested without a NATS connection or a
// package manager.
type PatchDeps struct {
	// Manager returns the OS update backend. It must return the SAME
	// backend every call: on Windows the manager owns a locked OS thread
	// with an initialised COM apartment.
	Manager func() (patch.Manager, error)

	// RefreshPatchState re-scans and republishes the patchstate inventory
	// snapshot, returning what it published.
	RefreshPatchState func(context.Context) (inventory.PatchState, error)

	// Runner publishes job progress, output and results.
	Runner *scripts.Runner

	Audit *audit.Publisher
	Log   *slog.Logger
}

// Execute runs one patch.* job end to end: progress, a human-readable
// summary on the output stream, the terminal result and an audit event.
// It never returns an error, for the same reason scripts.Runner.Run does
// not: every failure mode is a Result the server can display.
func (d PatchDeps) Execute(ctx context.Context, spec scripts.JobSpec, progress scripts.ProgressFunc) scripts.Result {
	if progress == nil {
		progress = func(int, string, string) {}
	}
	started := time.Now()
	progress(0, scripts.PhaseStarted, spec.Kind)

	ctx, cancel := context.WithTimeout(ctx, patchJobTimeout(spec))
	defer cancel()

	var (
		summary   string
		err       error
		failed    int
		installed patch.InstallResult
	)
	switch spec.Kind {
	case scripts.KindPatchScan:
		var state inventory.PatchState
		state, err = HandlePatchScan(ctx, d, progress)
		summary = scanSummary(state, err)
		d.emit(EventPatchScanned, map[string]any{
			"job_id":          spec.JobID,
			"backend":         state.Backend,
			"available":       len(state.Patches),
			"reboot_required": state.RebootRequired,
			"requested_by":    spec.RequestedBy,
			"ok":              err == nil,
		})
	case scripts.KindPatchInstall:
		ids := ParsePatchIDs(spec.Body)
		var out patch.InstallResult
		out, err = HandlePatchInstall(ctx, d, ids, progress)
		failed = len(out.Failed)
		installed = out
		summary = installSummary(ids, out, err)
		d.emit(EventPatchInstalled, map[string]any{
			"job_id":          spec.JobID,
			"requested":       len(ids),
			"installed":       out.Installed,
			"failed":          out.Failed,
			"reboot_required": out.RebootRequired,
			"requested_by":    spec.RequestedBy,
		})
	default:
		err = fmt.Errorf("patch handler cannot run job kind %q", spec.Kind)
		summary = err.Error()
	}

	res := patchResult(err, failed)
	res.DurationMS = time.Since(started).Milliseconds()
	// The job result is the authoritative record of what happened. Carrying
	// only status/exit_code left the server storing an empty installed list
	// for a job that had genuinely installed something, so an operator could
	// not tell WHICH updates landed without reading the audit stream.
	if spec.Kind == scripts.KindPatchInstall {
		res.Installed = installed.Installed
		res.Failed = installed.Failed
		res.RebootRequired = installed.RebootRequired
	}

	// One PublishStderr call per job: it emits both EOF markers, so a
	// second call would reopen a stream the server has already closed.
	if d.Runner != nil {
		d.Runner.PublishStderr(spec, "openrmm-agent: "+summary+"\n")
	}
	progress(100, scripts.PhaseFinished, res.Status)
	if d.Runner != nil {
		d.Runner.PublishResult(spec, res)
	}
	return res
}

// HandlePatchScan runs a scan, publishes the patchstate inventory snapshot,
// and reports what it found. Progress is advisory: the scan itself is one
// long call inside the backend with nothing to report from.
func HandlePatchScan(ctx context.Context, deps PatchDeps, progress scripts.ProgressFunc) (inventory.PatchState, error) {
	if deps.RefreshPatchState == nil {
		return inventory.PatchState{}, errors.New("patch scan is not wired: RefreshPatchState is nil")
	}
	if progress != nil {
		progress(10, scripts.PhaseRunning, "scanning for updates")
	}
	state, err := deps.RefreshPatchState(ctx)
	if err != nil {
		deps.warn("patch scan failed", "err", err)
		return state, err
	}
	deps.info("patch scan complete",
		"backend", state.Backend,
		"available", len(state.Patches),
		"reboot_required", state.RebootRequired)
	return state, nil
}

// HandlePatchInstall installs the approved update ids, streaming progress,
// and republishes the patchstate snapshot afterwards so the console shows
// the post-install truth without waiting for the next cycle.
//
// The refresh happens even when the install failed: a partial install still
// changes what is pending.
func HandlePatchInstall(ctx context.Context, deps PatchDeps, ids []string, progress scripts.ProgressFunc) (patch.InstallResult, error) {
	if len(ids) == 0 {
		return patch.InstallResult{Installed: []string{}, Failed: map[string]string{}},
			errors.New("patch install job carried no update ids")
	}
	if deps.Manager == nil {
		return patch.InstallResult{}, errors.New("patch install is not wired: Manager is nil")
	}
	mgr, err := deps.Manager()
	if err != nil {
		return patch.InstallResult{}, err
	}

	if progress != nil {
		progress(5, scripts.PhaseRunning, fmt.Sprintf("installing %d update(s)", len(ids)))
	}
	res, err := mgr.Install(ctx, ids, func(p patch.InstallProgress) {
		if progress == nil {
			return
		}
		note := p.Phase
		if p.UpdateID != "" {
			note = p.Phase + " " + p.UpdateID
		}
		progress(p.Pct, scripts.PhaseRunning, note)
	})

	// Refresh regardless of the outcome, and do not let a refresh failure
	// mask the install error the operator actually needs to see.
	if deps.RefreshPatchState != nil {
		if _, rerr := deps.RefreshPatchState(ctx); rerr != nil {
			deps.warn("patchstate refresh after install failed", "err", rerr)
		}
	}
	if err != nil {
		deps.warn("patch install failed", "backend", mgr.Kind(), "err", err)
		return res, err
	}
	deps.info("patch install complete",
		"backend", mgr.Kind(),
		"installed", len(res.Installed),
		"failed", len(res.Failed),
		"reboot_required", res.RebootRequired)
	return res, nil
}

// ParsePatchIDs pulls approved update ids out of a job body. The server may
// send a JSON object, a bare JSON array, or a newline-separated list.
//
// Plain text is split on NEWLINES only, never on spaces: a macOS update
// label is "macOS Sequoia 15.5-24F74", and splitting that on whitespace
// would turn one update into three that do not exist.
func ParsePatchIDs(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}

	// A body that opens as JSON is only read as JSON. Falling through to
	// the plain-text split would turn an object we did not understand into
	// one bogus update id.
	switch body[0] {
	case '{':
		var obj struct {
			UpdateIDs []string `json:"update_ids"`
			IDs       []string `json:"ids"`
			Patches   []string `json:"patches"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			return nil
		}
		for _, list := range [][]string{obj.UpdateIDs, obj.IDs, obj.Patches} {
			if len(list) > 0 {
				return cleanIDs(list)
			}
		}
		return nil
	case '[':
		var arr []string
		if err := json.Unmarshal([]byte(body), &arr); err != nil {
			return nil
		}
		return cleanIDs(arr)
	default:
		return cleanIDs(strings.Split(body, "\n"))
	}
}

func cleanIDs(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		if id = strings.TrimSpace(strings.TrimSuffix(id, "\r")); id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// patchJobTimeout picks the job deadline, preferring the server's value.
func patchJobTimeout(spec scripts.JobSpec) time.Duration {
	if spec.TimeoutS > 0 {
		if d := time.Duration(spec.TimeoutS) * time.Second; d < scripts.MaxTimeout {
			return d
		}
		return scripts.MaxTimeout
	}
	if spec.Kind == scripts.KindPatchInstall {
		return defaultInstallTimeout
	}
	return defaultScanTimeout
}

// patchResult maps an error and a per-update failure count onto the wire
// status. A partially failed install is a failed job: the operator asked
// for ten updates and got eight.
func patchResult(err error, failed int) scripts.Result {
	switch {
	case errors.Is(err, patch.ErrBusy):
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: exitBusy}
	case errors.Is(err, context.DeadlineExceeded):
		return scripts.Result{Status: scripts.StatusTimeout, ExitCode: -1}
	case errors.Is(err, context.Canceled):
		return scripts.Result{Status: scripts.StatusCancelled, ExitCode: -1}
	case err != nil:
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: -1}
	case failed > 0:
		return scripts.Result{Status: scripts.StatusFailed, ExitCode: failed}
	default:
		return scripts.Result{Status: scripts.StatusSucceeded}
	}
}

// scanSummary is the one line an operator reads in the job console.
func scanSummary(state inventory.PatchState, err error) string {
	if err != nil {
		return "patch scan failed: " + err.Error()
	}
	counts := map[string]int{}
	unsupported := 0
	for _, p := range state.Patches {
		counts[p.Kind]++
		if p.Unsupported {
			unsupported++
		}
	}
	summary := fmt.Sprintf("patch scan via %s: %d update(s) available",
		state.Backend, len(state.Patches))
	if len(counts) > 0 {
		summary += " (" + describeCounts(counts) + ")"
	}
	if unsupported > 0 {
		summary += fmt.Sprintf("; %d cannot be installed by the agent", unsupported)
	}
	if state.RebootRequired {
		summary += "; a reboot is already pending"
	}
	return summary
}

// installSummary reports what went on and what did not, naming the failures
// so the console does not require a log dive.
func installSummary(ids []string, res patch.InstallResult, err error) string {
	summary := fmt.Sprintf("patch install: %d of %d requested update(s) installed",
		len(res.Installed), len(ids))
	if len(res.Failed) > 0 {
		failures := make([]string, 0, len(res.Failed))
		for id, reason := range res.Failed {
			failures = append(failures, id+": "+reason)
		}
		sort.Strings(failures)
		summary += "; failed: " + strings.Join(failures, "; ")
	}
	if res.RebootRequired {
		summary += "; a reboot is required to finish"
	}
	if err != nil {
		summary += "; error: " + err.Error()
	}
	return summary
}

// describeCounts renders a kind histogram in a stable order.
func describeCounts(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	return strings.Join(parts, ", ")
}

func (d PatchDeps) emit(event string, detail map[string]any) {
	if d.Audit != nil {
		d.Audit.Emit(event, detail)
	}
}

func (d PatchDeps) info(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Info(msg, args...)
	}
}

func (d PatchDeps) warn(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Warn(msg, args...)
	}
}
