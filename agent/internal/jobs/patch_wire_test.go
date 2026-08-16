package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rsp2k/openrmm/agent/internal/inventory"
	"github.com/rsp2k/openrmm/agent/internal/patch"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
)

// fakeManager is a patch.Manager that reports whatever a test tells it to.
type fakeManager struct {
	kind        string
	scanned     []patch.Update
	scanErr     error
	installRes  patch.InstallResult
	installErr  error
	installedID []string
	progress    []patch.InstallProgress
}

func (f *fakeManager) Kind() string { return f.kind }

func (f *fakeManager) Scan(context.Context) ([]patch.Update, error) {
	return f.scanned, f.scanErr
}

func (f *fakeManager) Install(_ context.Context, ids []string, progress func(patch.InstallProgress)) (patch.InstallResult, error) {
	f.installedID = ids
	for _, p := range f.progress {
		progress(p)
	}
	return f.installRes, f.installErr
}

func (f *fakeManager) RebootRequired(context.Context) (bool, error) { return false, nil }

func testPatchDeps(t *testing.T, mgr *fakeManager, state inventory.PatchState, stateErr error) PatchDeps {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return PatchDeps{
		Manager: func() (patch.Manager, error) { return mgr, nil },
		RefreshPatchState: func(context.Context) (inventory.PatchState, error) {
			return state, stateErr
		},
		Runner: scripts.NewRunner(nil, "agent-1", t.TempDir(), nil, log),
		Log:    log,
	}
}

func TestParsePatchIDs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "json object with update_ids",
			body: `{"update_ids":["libc6=2.31-13","systemd=255.7-1"]}`,
			want: []string{"libc6=2.31-13", "systemd=255.7-1"},
		},
		{
			name: "json object with ids",
			body: `{"ids":["KB5034123"]}`,
			want: []string{"KB5034123"},
		},
		{
			name: "bare json array",
			body: `["7d5a4c1e-9c8b-4b2a-9e4f-1a2b3c4d5e6f"]`,
			want: []string{"7d5a4c1e-9c8b-4b2a-9e4f-1a2b3c4d5e6f"},
		},
		{
			name: "newline separated plain text",
			body: "libc6=2.31-13\nsystemd=255.7-1\n",
			want: []string{"libc6=2.31-13", "systemd=255.7-1"},
		},
		{
			name: "macos labels keep their spaces",
			body: "macOS Sequoia 15.5-24F74\nSafari18.5Sequoia-18.5",
			want: []string{"macOS Sequoia 15.5-24F74", "Safari18.5Sequoia-18.5"},
		},
		{
			name: "macos labels through json",
			body: `{"update_ids":["macOS Sequoia 15.5-24F74"]}`,
			want: []string{"macOS Sequoia 15.5-24F74"},
		},
		{
			name: "windows line endings",
			body: "libc6=2.31-13\r\nsystemd=255.7-1\r\n",
			want: []string{"libc6=2.31-13", "systemd=255.7-1"},
		},
		{name: "empty", body: "", want: nil},
		{name: "whitespace only", body: "   \n\t\n", want: nil},
		{name: "empty json array", body: `[]`, want: nil},
		{name: "object with no id fields", body: `{"other":1}`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParsePatchIDs(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParsePatchIDs(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestPatchResult(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		failed   int
		status   string
		exitCode int
	}{
		{name: "clean", status: scripts.StatusSucceeded},
		{name: "busy is retryable", err: patch.ErrBusy, status: scripts.StatusFailed, exitCode: exitBusy},
		{name: "wrapped busy", err: errors.New("x: " + patch.ErrBusy.Error()), status: scripts.StatusFailed, exitCode: -1},
		{name: "deadline", err: context.DeadlineExceeded, status: scripts.StatusTimeout, exitCode: -1},
		{name: "cancelled", err: context.Canceled, status: scripts.StatusCancelled, exitCode: -1},
		{name: "generic failure", err: errors.New("boom"), status: scripts.StatusFailed, exitCode: -1},
		{name: "partial install", failed: 2, status: scripts.StatusFailed, exitCode: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := patchResult(tc.err, tc.failed)
			if got.Status != tc.status || got.ExitCode != tc.exitCode {
				t.Errorf("patchResult = %+v, want status %q exit %d", got, tc.status, tc.exitCode)
			}
		})
	}
}

func TestPatchJobTimeout(t *testing.T) {
	tests := []struct {
		name string
		spec scripts.JobSpec
		want time.Duration
	}{
		{name: "scan default", spec: scripts.JobSpec{Kind: scripts.KindPatchScan}, want: defaultScanTimeout},
		{name: "install default", spec: scripts.JobSpec{Kind: scripts.KindPatchInstall}, want: defaultInstallTimeout},
		{name: "server value wins", spec: scripts.JobSpec{Kind: scripts.KindPatchScan, TimeoutS: 60}, want: time.Minute},
		{name: "clamped", spec: scripts.JobSpec{Kind: scripts.KindPatchInstall, TimeoutS: 999999999}, want: scripts.MaxTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := patchJobTimeout(tc.spec); got != tc.want {
				t.Errorf("patchJobTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestHandlePatchScan(t *testing.T) {
	state := inventory.PatchState{
		Backend:        "apt",
		RebootRequired: true,
		Patches: []inventory.PatchEntry{
			{ID: "libc6=2.31-13", Kind: "security"},
			{ID: "vim=2:8.2", Kind: "other"},
		},
	}
	deps := testPatchDeps(t, &fakeManager{kind: "apt"}, state, nil)
	got, err := HandlePatchScan(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got.Patches) != 2 || got.Backend != "apt" {
		t.Errorf("state = %+v", got)
	}
}

func TestHandlePatchScanUnwired(t *testing.T) {
	_, err := HandlePatchScan(context.Background(), PatchDeps{}, nil)
	if err == nil {
		t.Fatal("an unwired scan must report an error, not a clean empty snapshot")
	}
}

func TestHandlePatchInstall(t *testing.T) {
	mgr := &fakeManager{
		kind: "apt",
		installRes: patch.InstallResult{
			Installed:      []string{"libc6=2.31-13"},
			Failed:         map[string]string{"vim=2:8.2": "held back"},
			RebootRequired: true,
		},
		progress: []patch.InstallProgress{
			{Phase: patch.PhaseDownload, Pct: 10},
			{UpdateID: "libc6=2.31-13", Phase: patch.PhaseInstall, Pct: 50},
		},
	}
	refreshed := 0
	deps := testPatchDeps(t, mgr, inventory.PatchState{Backend: "apt"}, nil)
	inner := deps.RefreshPatchState
	deps.RefreshPatchState = func(ctx context.Context) (inventory.PatchState, error) {
		refreshed++
		return inner(ctx)
	}

	var ticks []string
	res, err := HandlePatchInstall(context.Background(), deps,
		[]string{"libc6=2.31-13", "vim=2:8.2"},
		func(pct int, phase, note string) { ticks = append(ticks, note) })
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !reflect.DeepEqual(mgr.installedID, []string{"libc6=2.31-13", "vim=2:8.2"}) {
		t.Errorf("ids passed to the backend = %v", mgr.installedID)
	}
	if len(res.Installed) != 1 || len(res.Failed) != 1 || !res.RebootRequired {
		t.Errorf("result = %+v", res)
	}
	if refreshed != 1 {
		t.Errorf("patchstate refreshed %d times, want 1", refreshed)
	}
	if len(ticks) < 3 {
		t.Errorf("progress notes = %v, want the start tick plus both backend ticks", ticks)
	}
	if !strings.Contains(strings.Join(ticks, "|"), "libc6=2.31-13") {
		t.Errorf("per-update progress did not name the update: %v", ticks)
	}
}

// TestHandlePatchInstallRefreshesAfterFailure pins down the rule that a
// failed install still republishes patchstate: a partial install changes
// what is pending, and the console must not keep showing the old list.
func TestHandlePatchInstallRefreshesAfterFailure(t *testing.T) {
	mgr := &fakeManager{kind: "dnf", installErr: errors.New("transaction failed")}
	refreshed := 0
	deps := testPatchDeps(t, mgr, inventory.PatchState{Backend: "dnf"}, nil)
	deps.RefreshPatchState = func(context.Context) (inventory.PatchState, error) {
		refreshed++
		return inventory.PatchState{Backend: "dnf"}, nil
	}
	if _, err := HandlePatchInstall(context.Background(), deps, []string{"kernel.x86_64=1"}, nil); err == nil {
		t.Fatal("expected the install error to propagate")
	}
	if refreshed != 1 {
		t.Errorf("patchstate refreshed %d times after a failed install, want 1", refreshed)
	}
}

func TestHandlePatchInstallNoIDs(t *testing.T) {
	deps := testPatchDeps(t, &fakeManager{kind: "apt"}, inventory.PatchState{}, nil)
	if _, err := HandlePatchInstall(context.Background(), deps, nil, nil); err == nil {
		t.Fatal("an install with no ids must be an error, not a silent success")
	}
}

func TestExecutePatchScan(t *testing.T) {
	state := inventory.PatchState{
		Backend: "apt",
		Patches: []inventory.PatchEntry{{ID: "a", Kind: "security"}},
	}
	deps := testPatchDeps(t, &fakeManager{kind: "apt"}, state, nil)
	res := deps.Execute(context.Background(),
		scripts.JobSpec{JobID: "j1", Kind: scripts.KindPatchScan}, nil)
	if res.Status != scripts.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
}

func TestExecutePatchInstallBusyIsRetryable(t *testing.T) {
	mgr := &fakeManager{kind: "wua", installErr: patch.ErrBusy}
	deps := testPatchDeps(t, mgr, inventory.PatchState{Backend: "wua"}, nil)
	res := deps.Execute(context.Background(), scripts.JobSpec{
		JobID: "j2", Kind: scripts.KindPatchInstall, Body: `{"update_ids":["KB1"]}`,
	}, nil)
	if res.Status != scripts.StatusFailed || res.ExitCode != exitBusy {
		t.Errorf("result = %+v, want a failed status with exit %d", res, exitBusy)
	}
}

func TestExecuteUnknownKind(t *testing.T) {
	deps := testPatchDeps(t, &fakeManager{kind: "apt"}, inventory.PatchState{}, nil)
	res := deps.Execute(context.Background(), scripts.JobSpec{JobID: "j3", Kind: "script.run"}, nil)
	if res.Status != scripts.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
}

func TestScanSummary(t *testing.T) {
	state := inventory.PatchState{
		Backend:        "apt",
		RebootRequired: true,
		Patches: []inventory.PatchEntry{
			{Kind: "security"},
			{Kind: "security"},
			{Kind: "other", Unsupported: true},
		},
	}
	got := scanSummary(state, nil)
	for _, want := range []string{"apt", "3 update(s)", "2 security", "1 other", "1 cannot be installed", "reboot is already pending"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
	if failed := scanSummary(inventory.PatchState{}, errors.New("no mirror")); !strings.Contains(failed, "no mirror") {
		t.Errorf("failure summary = %q", failed)
	}
}

func TestInstallSummary(t *testing.T) {
	res := patch.InstallResult{
		Installed:      []string{"a"},
		Failed:         map[string]string{"b": "held back", "c": "not found"},
		RebootRequired: true,
	}
	got := installSummary([]string{"a", "b", "c"}, res, nil)
	for _, want := range []string{"1 of 3", "b: held back", "c: not found", "reboot is required"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
	// Failure order must be stable so two identical runs read identically.
	if got != installSummary([]string{"a", "b", "c"}, res, nil) {
		t.Error("install summary is not deterministic")
	}
}

func TestDescribeCounts(t *testing.T) {
	got := describeCounts(map[string]int{"security": 2, "other": 1, "definition": 5})
	want := "5 definition, 1 other, 2 security"
	if got != want {
		t.Errorf("describeCounts = %q, want %q", got, want)
	}
}
