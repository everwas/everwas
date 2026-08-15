// Package patch discovers and installs operating system updates. Every OS
// gets a Manager implementation behind the same interface: Windows through
// the Windows Update Agent COM API, Debian/Ubuntu through apt, RHEL/Fedora
// through dnf, Arch through pacman (best effort), macOS through
// softwareupdate.
//
// Two rules hold for every backend. The agent never reboots a host on its
// own: a Manager reports RebootRequired and the decision stays with the
// operator. And a scan is best effort: a backend that cannot classify one
// update degrades that field rather than failing the whole snapshot.
package patch

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
)

// Update kinds. An update the backend cannot classify is KindOther, never
// an empty string, so the server never has to guess what "" meant.
const (
	KindSecurity   = "security"
	KindFeature    = "feature"
	KindDefinition = "definition"
	KindOther      = "other"
)

// Severities, most to least urgent. Backends that do not publish a severity
// (apt, pacman, softwareupdate) report SeverityUnknown rather than inventing
// one from the package name.
const (
	SeverityCritical  = "critical"
	SeverityImportant = "important"
	SeverityModerate  = "moderate"
	SeverityLow       = "low"
	SeverityUnknown   = "unknown"
)

// Install phases carried on InstallProgress.
const (
	PhaseDownload = "download"
	PhaseInstall  = "install"
)

// Backend kinds, as reported by Manager.Kind and published in the patchstate
// inventory snapshot.
const (
	BackendWUA            = "wua"
	BackendAPT            = "apt"
	BackendDNF            = "dnf"
	BackendPacman         = "pacman"
	BackendSoftwareUpdate = "softwareupdate"
)

// ErrBusy means another install is already running, either ours or one
// started outside the agent (Windows Update in the UI, unattended-upgrades,
// a human at a dnf prompt). It is retryable: the server can requeue the job.
var ErrBusy = errors.New("patch: another install is already in progress")

// ErrUnsupported means this host has no update backend the agent can drive.
var ErrUnsupported = errors.New("patch: no supported update backend on this host")

// Update is one available update, in backend-native terms.
type Update struct {
	// ID is what Install takes back: a WUA UpdateID GUID on Windows,
	// "pkg=version" for apt, "name.arch=evr" for dnf, "pkg=version" for
	// pacman, the label for softwareupdate.
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind"`
	KBIDs        []string `json:"kb_ids,omitempty"`
	Severity     string   `json:"severity"`
	SizeBytes    int64    `json:"size_bytes"`
	RebootLikely bool     `json:"reboot_likely"`

	// Unsupported marks an update we can see but cannot install from a
	// headless agent. It is still reported so the operator knows the host is
	// behind; Detail says why we will not touch it.
	Unsupported bool   `json:"unsupported"`
	Detail      string `json:"detail,omitempty"`
}

// InstallProgress is one progress tick during an install. UpdateID is empty
// for phase transitions that cover the whole batch.
type InstallProgress struct {
	UpdateID string
	Phase    string
	Pct      int
}

// InstallResult is the outcome of one Install call. Installed and Failed
// together account for every id that was asked for.
type InstallResult struct {
	Installed      []string          `json:"installed"`
	Failed         map[string]string `json:"failed"`
	RebootRequired bool              `json:"reboot_required"`
}

// newInstallResult returns a result with non-nil maps and slices, so the
// JSON shape is stable ([] and {}, never null).
func newInstallResult() InstallResult {
	return InstallResult{Installed: []string{}, Failed: map[string]string{}}
}

// fail records a per-update failure without clobbering an earlier one.
func (r *InstallResult) fail(id string, err error) {
	if r.Failed == nil {
		r.Failed = map[string]string{}
	}
	if _, seen := r.Failed[id]; seen {
		return
	}
	r.Failed[id] = err.Error()
}

// Manager is the per-OS update backend.
type Manager interface {
	// Kind names the backend: wua, apt, dnf, pacman, softwareupdate.
	Kind() string

	// Scan lists updates that are available and not yet installed. A scan
	// can take minutes (a WUA search routinely takes 2 to 10), so callers
	// must not impose a short timeout.
	Scan(ctx context.Context) ([]Update, error)

	// Install installs the given ids. progress may be nil. It returns
	// ErrBusy if another install holds the backend.
	Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error)

	// RebootRequired reports whether the host is waiting on a reboot to
	// finish applying updates. The agent never acts on this by itself.
	RebootRequired(ctx context.Context) (bool, error)
}

// Logged is implemented by backends that can report degraded steps (a
// mirror that would not refresh, an update block that would not parse).
// Detect stays parameterless as the interface documents; WithLogger is how
// a caller opts into the detail.
type Logged interface {
	SetLogger(*slog.Logger)
}

// WithLogger attaches log to m when the backend supports it, and returns m
// either way so it can be used inline.
func WithLogger(m Manager, log *slog.Logger) Manager {
	if l, ok := m.(Logged); ok && log != nil {
		l.SetLogger(log)
	}
	return m
}

// loggerHolder is embedded by every backend to satisfy Logged.
type loggerHolder struct {
	log *slog.Logger
}

func (h *loggerHolder) SetLogger(log *slog.Logger) { h.log = log }

// logger never returns nil, so backends can log unconditionally.
func (h *loggerHolder) logger() *slog.Logger {
	if h.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.log
}

// installGate serialises Install across callers within this process. Every
// backend embeds one; a second concurrent Install gets ErrBusy instead of
// two package managers fighting over the same lock file.
type installGate struct {
	held atomic.Bool
}

// acquire takes the gate, reporting whether it was free.
func (g *installGate) acquire() bool { return g.held.CompareAndSwap(false, true) }

func (g *installGate) release() { g.held.Store(false) }

// emitProgress calls progress if it is set. Backends call this rather than
// nil-checking at every tick.
func emitProgress(progress func(InstallProgress), p InstallProgress) {
	if progress != nil {
		progress(p)
	}
}

// dedupeIDs removes duplicates and empty ids while preserving order, so a
// server that sends the same update twice does not install it twice.
func dedupeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
