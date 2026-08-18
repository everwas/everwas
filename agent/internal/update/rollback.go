package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Rollback and probation policy.
const (
	// MinProbation is the shortest a freshly swapped build stays on
	// probation. Staying alive for a minute is not evidence of anything:
	// the supervisor restarts crashed tasks forever with backoff, so a
	// build whose jobs module panics on every start clears sixty seconds
	// without ever doing a unit of work.
	MinProbation = 10 * time.Minute

	// MaxProbation is when the crash counter is disarmed even though the
	// build never produced evidence. An agent that has run for a day
	// without proving itself is a monitoring problem, not a reason to keep
	// a rollback armed indefinitely. The backup is kept either way, so this
	// is not a point of no return.
	MaxProbation = 24 * time.Hour

	// ProbeInterval is how often the health probe is retried while a build
	// is on probation.
	ProbeInterval = 30 * time.Second

	// CrashWindow is how long after a swap we keep counting restarts.
	CrashWindow = 2 * time.Minute

	// CrashLimit is the number of extra starts inside CrashWindow tolerated
	// before the previous binary is restored. Start one is the normal
	// post-update launch; starts two and three are the two crashes.
	CrashLimit = 2

	stateFileName    = "update-state.json"
	healthMarkerName = "health-ok"

	// ProbationFileName is the plain text view of an in-flight update, read
	// by the EXTERNAL guard (packaging/linux/agent-guard.sh). The guard is
	// /bin/sh and must not have to parse JSON to decide whether a build is
	// on probation.
	ProbationFileName = "update-probation"

	// StartsFileName is where the external guard appends one unix timestamp
	// per launch. It exists because the agent's own counter can only be
	// incremented by a binary that got far enough to run, which excludes
	// every failure that matters most: wrong architecture, missing symbol,
	// panic in package init, a config format the new build cannot parse.
	StartsFileName = "update-starts"

	// DeniedFileName is the guard's half of the rolled-back denylist, one
	// version per line. The agent folds it into the state file at startup.
	DeniedFileName = "update-denied"

	maxStartsTracked = 16
	maxDeniedTracked = 32
)

// Self-update status values. Finalizing is deliberately NOT terminal: the
// swap has been handed to a helper process and has not happened yet, so a
// server that files it as "applied" is recording an update the host may
// never receive.
const (
	StatusIdle           = "idle"
	StatusProbation      = "probation"
	StatusFinalizing     = "finalizing"
	StatusFinalizeFailed = "finalize_failed"
	StatusRolledBack     = "rolled_back"
	StatusHealthy        = "healthy"
	StatusUnproven       = "unproven"
)

// ErrState wraps failures to read or write the update state file.
var ErrState = errors.New("update: state file")

// ErrFinalizePending means an out-of-process finalizer was spawned and has
// not reported an outcome yet, so nothing about this update is settled.
var ErrFinalizePending = errors.New("update: finalizer has not reported yet")

// State is the on-disk record of an in-flight update.
type State struct {
	PendingVersion  string      `json:"pending_version,omitempty"`
	PreviousVersion string      `json:"previous_version,omitempty"`
	Target          string      `json:"target,omitempty"`
	Backup          string      `json:"backup,omitempty"`
	SwappedAt       time.Time   `json:"swapped_at,omitempty"`
	Starts          []time.Time `json:"starts,omitempty"`
	Healthy         bool        `json:"healthy"`
	HealthyAt       time.Time   `json:"healthy_at,omitempty"`
	RolledBack      bool        `json:"rolled_back,omitempty"`

	// Unproven records that probation ended on the clock rather than on
	// evidence: the build never satisfied the health probe. The backup is
	// still on disk, so this is a report, not a failure.
	Unproven bool `json:"unproven,omitempty"`

	// Denied is the list of versions that were rolled back on this host.
	// Re-applying one without an explicit force flag is refused, which is
	// what stops a fleet from downloading, swapping, crashing and rolling
	// back on a loop against a bad release.
	Denied []string `json:"denied,omitempty"`

	// Finalizing marks a swap handed to an out-of-process helper (the
	// Windows fallback). It stays set until that helper reports back, and
	// while it is set nothing may be declared healthy.
	Finalizing   bool      `json:"finalizing,omitempty"`
	FinalizerPID int       `json:"finalizer_pid,omitempty"`
	FinalizeErr  string    `json:"finalize_error,omitempty"`
	FinalizedAt  time.Time `json:"finalized_at,omitempty"`
}

// Pending reports whether an update is still on probation.
func (s State) Pending() bool { return s.PendingVersion != "" && !s.Healthy }

// Report is the agent's self-update status, shaped for publishing. Terminal
// says whether the server can stop tracking this update.
type Report struct {
	Status   string    `json:"status"`
	Version  string    `json:"version,omitempty"`
	Previous string    `json:"previous_version,omitempty"`
	Since    time.Time `json:"since,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Denied   []string  `json:"denied,omitempty"`
	Terminal bool      `json:"terminal"`
}

// Tracker owns the update state file inside a state directory. Now is
// injectable so the rollback decision can be tested without sleeping.
type Tracker struct {
	StateDir string
	Now      func() time.Time
}

// NewTracker returns a Tracker backed by the real clock.
func NewTracker(stateDir string) *Tracker {
	return &Tracker{StateDir: stateDir, Now: time.Now}
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tracker) statePath() string     { return filepath.Join(t.StateDir, stateFileName) }
func (t *Tracker) markerPath() string    { return filepath.Join(t.StateDir, healthMarkerName) }
func (t *Tracker) probationPath() string { return filepath.Join(t.StateDir, ProbationFileName) }
func (t *Tracker) startsPath() string    { return filepath.Join(t.StateDir, StartsFileName) }
func (t *Tracker) deniedPath() string    { return filepath.Join(t.StateDir, DeniedFileName) }

// Load reads the state file. A missing file is not an error: it means no
// update is in flight.
func (t *Tracker) Load() (State, error) {
	raw, err := os.ReadFile(t.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("%w: read: %v", ErrState, err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		// A corrupt state file must not wedge the agent at startup. Treat it
		// as "no update in flight" and let the next update rewrite it.
		return State{}, nil
	}
	return st, nil
}

// Save writes the state file with the same rename-into-place discipline the
// identity file uses, so a crash mid-write cannot truncate it.
func (t *Tracker) Save(st State) error {
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrState, err)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode: %v", ErrState, err)
	}
	tmp := t.statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("%w: write: %v", ErrState, err)
	}
	if err := os.Rename(tmp, t.statePath()); err != nil {
		return fmt.Errorf("%w: rename: %v", ErrState, err)
	}
	return nil
}

// Clear removes the state file, the health marker and the guard's view of
// the probation. The denylist survives on purpose: it is the record of what
// this host must not be sent again.
func (t *Tracker) Clear() error {
	for _, p := range []string{t.statePath(), t.markerPath(), t.probationPath(), t.startsPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: remove %s: %v", ErrState, filepath.Base(p), err)
		}
	}
	return nil
}

// BeginUpdate records an update as in flight. Call it immediately before the
// swap so that a crash between the two leaves a harmless orphan record
// instead of an unrecoverable one.
//
// The denylist is carried across: an update that wipes the record of what
// was already rolled back re-enables the loop that record exists to stop.
func (t *Tracker) BeginUpdate(newVersion, prevVersion, target, backup string) error {
	prior, err := t.Load()
	if err != nil {
		return err
	}
	_ = os.Remove(t.markerPath())
	_ = os.Remove(t.startsPath())
	st := State{
		PendingVersion:  newVersion,
		PreviousVersion: prevVersion,
		Target:          target,
		Backup:          backup,
		SwappedAt:       t.now(),
		Denied:          prior.Denied,
	}
	if err := t.Save(st); err != nil {
		return err
	}
	return t.writeProbation(st)
}

// BeginFinalize records that the swap was handed to an out-of-process
// helper. Until that helper reports, the update has NOT been applied and
// must not be declared healthy: the running binary is still the old one.
func (t *Tracker) BeginFinalize(pid int) error {
	st, err := t.Load()
	if err != nil {
		return err
	}
	st.Finalizing = true
	st.FinalizerPID = pid
	st.FinalizeErr = ""
	st.FinalizedAt = time.Time{}
	if err := t.Save(st); err != nil {
		return err
	}
	return t.writeProbation(st)
}

// FinalizeOutcome is written by the finalizer process itself, which is a
// different process from the agent. A finalizer that gave up has to say so:
// silence there is what let a host stay on the old version while the console
// showed the update as applied.
func (t *Tracker) FinalizeOutcome(failure error) error {
	st, err := t.Load()
	if err != nil {
		return err
	}
	st.Finalizing = false
	st.FinalizedAt = t.now()
	if failure != nil {
		st.FinalizeErr = failure.Error()
	} else {
		st.FinalizeErr = ""
	}
	if err := t.Save(st); err != nil {
		return err
	}
	if failure != nil {
		// The swap did not happen, so nothing is on probation and the guard
		// must not count starts of a binary that never changed.
		if err := os.Remove(t.probationPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: remove probation: %v", ErrState, err)
		}
		return nil
	}
	return t.writeProbation(st)
}

// writeProbation renders the plain text file the external guard reads. It is
// key=value on purpose: the guard is /bin/sh.
func (t *Tracker) writeProbation(st State) error {
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrState, err)
	}
	var b strings.Builder
	b.WriteString("# written by everwas-agent, read by agent-guard.sh\n")
	fmt.Fprintf(&b, "version=%s\n", st.PendingVersion)
	fmt.Fprintf(&b, "previous=%s\n", st.PreviousVersion)
	fmt.Fprintf(&b, "target=%s\n", st.Target)
	fmt.Fprintf(&b, "backup=%s\n", st.Backup)
	fmt.Fprintf(&b, "swapped_at=%d\n", st.SwappedAt.Unix())
	fmt.Fprintf(&b, "finalizing=%t\n", st.Finalizing)
	tmp := t.probationPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("%w: write probation: %v", ErrState, err)
	}
	if err := os.Rename(tmp, t.probationPath()); err != nil {
		return fmt.Errorf("%w: rename probation: %v", ErrState, err)
	}
	return nil
}

// externalStarts reads the launch timestamps the guard appended. The guard
// runs from the service manager BEFORE the agent, so these cover the starts
// the agent never lived long enough to record for itself.
func (t *Tracker) externalStarts() []time.Time {
	raw, err := os.ReadFile(t.startsPath())
	if err != nil {
		return nil
	}
	var out []time.Time
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		secs, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, time.Unix(secs, 0))
	}
	if len(out) > maxStartsTracked {
		out = out[len(out)-maxStartsTracked:]
	}
	return out
}

// Denied reports every version this host has rolled back, merging the state
// file with the guard's append-only list.
func (t *Tracker) Denied() []string {
	st, err := t.Load()
	if err != nil {
		st = State{}
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range st.Denied {
		add(v)
	}
	if raw, err := os.ReadFile(t.deniedPath()); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			add(line)
		}
	}
	if len(out) > maxDeniedTracked {
		out = out[len(out)-maxDeniedTracked:]
	}
	return out
}

// IsDenied reports whether a version was rolled back on this host.
func (t *Tracker) IsDenied(version string) bool {
	if version == "" {
		return false
	}
	for _, v := range t.Denied() {
		if v == version {
			return true
		}
	}
	return false
}

// Deny records a version as rolled back, appending to the plain text list
// the guard also writes.
func (t *Tracker) Deny(version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrState, err)
	}
	f, err := os.OpenFile(t.deniedPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("%w: open denylist: %v", ErrState, err)
	}
	defer f.Close()
	if _, err := f.WriteString(version + "\n"); err != nil {
		return fmt.Errorf("%w: append denylist: %v", ErrState, err)
	}
	return nil
}

// RecordStart appends this launch to the state file when an update is
// pending. It returns the updated state. Launches outside an update window
// are not recorded, so the file stays absent on a normally running agent.
func (t *Tracker) RecordStart() (State, error) {
	st, err := t.Load()
	if err != nil {
		return State{}, err
	}
	if !st.Pending() {
		return st, nil
	}
	st.Starts = append(st.Starts, t.now())
	if len(st.Starts) > maxStartsTracked {
		st.Starts = st.Starts[len(st.Starts)-maxStartsTracked:]
	}
	if err := t.Save(st); err != nil {
		return st, err
	}
	return st, nil
}

// MarkHealthy declares the running build good. It writes the health marker
// and ends the probation, and it deliberately does NOT delete the previous
// binary: one spare generation costs about twenty megabytes and is the only
// recovery path this agent has for a defect that shows up on day two.
func (t *Tracker) MarkHealthy() error { return t.markHealthy(false) }

// MarkUnproven ends probation without evidence, once MaxProbation has
// passed. It is recorded as such so an operator can tell "it worked" from
// "it stopped being watched".
func (t *Tracker) MarkUnproven() error { return t.markHealthy(true) }

func (t *Tracker) markHealthy(unproven bool) error {
	st, err := t.Load()
	if err != nil {
		return err
	}
	if st.Finalizing {
		// The running process is still the OLD binary and the swap has not
		// happened. Declaring this healthy is exactly how a host stays on
		// the old version while the console says it updated.
		return fmt.Errorf("%w: version %s", ErrFinalizePending, st.PendingVersion)
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrState, err)
	}
	now := t.now()
	marker := fmt.Sprintf("version=%s\nhealthy_at=%s\nunproven=%t\n",
		st.PendingVersion, now.UTC().Format(time.RFC3339), unproven)
	if err := os.WriteFile(t.markerPath(), []byte(marker), 0o600); err != nil {
		return fmt.Errorf("%w: write marker: %v", ErrState, err)
	}
	if !st.Pending() {
		return nil
	}
	st.Healthy = true
	st.HealthyAt = now
	st.Unproven = unproven
	st.Starts = nil
	if err := t.Save(st); err != nil {
		return err
	}
	// Probation is over: stop the guard counting starts of a build it has
	// nothing left to say about. The backup stays where it is.
	for _, p := range []string{t.probationPath(), t.startsPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: remove %s: %v", ErrState, filepath.Base(p), err)
		}
	}
	return nil
}

// countStarts returns how many of these launches fall inside the crash
// window. Timestamps in the future are ignored: a clock jump must not look
// like a crash loop.
func countStarts(starts []time.Time, now time.Time) int {
	n := 0
	for _, s := range starts {
		if !s.After(now) && now.Sub(s) <= CrashWindow {
			n++
		}
	}
	return n
}

// shouldRollback is the whole rollback decision, kept pure so it can be
// tested without touching a filesystem or a clock.
//
// external is the guard's count, which is the one that matters for a build
// that dies before it can count for itself. Both lists describe the same
// launches from different sides, so the decision takes the larger of the two
// rather than the sum.
func shouldRollback(st State, external []time.Time, now time.Time) bool {
	if !st.Pending() || st.Backup == "" || st.Target == "" {
		return false
	}
	if st.RolledBack {
		return false
	}
	n := countStarts(st.Starts, now)
	if e := countStarts(external, now); e > n {
		n = e
	}
	return n > CrashLimit
}

// CheckAndRollback runs at startup, before anything else. It records this
// launch and, if the new build has crash looped, restores the previous
// binary and reports true. A true return means the caller should exit
// non-zero so the service manager starts the restored binary.
//
// It also reconciles what the external guard may already have done, so a
// rollback performed by the service manager before this process existed is
// still recorded and still denies the version.
func (t *Tracker) CheckAndRollback() (bool, error) {
	if done, err := t.reconcileGuardRollback(); done || err != nil {
		return false, err
	}
	st, err := t.RecordStart()
	if err != nil {
		return false, err
	}
	if !shouldRollback(st, t.externalStarts(), t.now()) {
		return false, nil
	}
	if err := RestoreBackup(st.Target); err != nil {
		// Nothing to restore, or the restore failed. Either way, stop
		// counting: repeating a rollback that cannot work just adds noise.
		st.RolledBack = true
		st.PendingVersion = ""
		if saveErr := t.finishRollback(st); saveErr != nil {
			return false, saveErr
		}
		return false, err
	}
	rolledBack := st.PendingVersion
	st.RolledBack = true
	st.Healthy = false
	st.PendingVersion = ""
	st.Starts = nil
	st.Denied = appendDenied(st.Denied, rolledBack)
	if err := t.Deny(rolledBack); err != nil {
		return true, err
	}
	if err := t.finishRollback(st); err != nil {
		return true, err
	}
	return true, nil
}

// finishRollback saves the state and clears the guard's files, so the
// restored binary starts from a clean count.
func (t *Tracker) finishRollback(st State) error {
	if err := t.Save(st); err != nil {
		return err
	}
	for _, p := range []string{t.probationPath(), t.startsPath()} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: remove %s: %v", ErrState, filepath.Base(p), err)
		}
	}
	return nil
}

// reconcileGuardRollback notices that the external guard already restored
// the previous binary. The guard cannot write JSON, so it appends the
// version to the denylist and deletes the probation file; this turns that
// into the same state a rollback performed here would have left.
//
// It reports true when it handled the situation, which means no crash
// counting should happen for this launch.
func (t *Tracker) reconcileGuardRollback() (bool, error) {
	st, err := t.Load()
	if err != nil {
		return false, err
	}
	if !st.Pending() || st.RolledBack {
		return false, nil
	}
	if _, err := os.Stat(t.probationPath()); !os.IsNotExist(err) {
		return false, nil // still on probation, or we cannot tell
	}
	if !t.IsDenied(st.PendingVersion) {
		return false, nil // the probation file went missing some other way
	}
	rolledBack := st.PendingVersion
	st.RolledBack = true
	st.Healthy = false
	st.PendingVersion = ""
	st.Starts = nil
	st.Denied = appendDenied(st.Denied, rolledBack)
	if err := t.finishRollback(st); err != nil {
		return true, err
	}
	return true, nil
}

func appendDenied(list []string, version string) []string {
	version = strings.TrimSpace(version)
	if version == "" {
		return list
	}
	for _, v := range list {
		if v == version {
			return list
		}
	}
	list = append(list, version)
	if len(list) > maxDeniedTracked {
		list = list[len(list)-maxDeniedTracked:]
	}
	return list
}

// Report describes where this host is in a self-update, for publishing to
// the server. Anything non-terminal has to be tracked to completion, which
// is the whole point of separating finalizing from applied.
func (t *Tracker) Report() (Report, error) {
	st, err := t.Load()
	if err != nil {
		return Report{Status: StatusIdle, Terminal: true}, err
	}
	rep := Report{
		Version:  st.PendingVersion,
		Previous: st.PreviousVersion,
		Since:    st.SwappedAt,
		Denied:   t.Denied(),
		Terminal: true,
	}
	switch {
	case st.Finalizing:
		rep.Status = StatusFinalizing
		rep.Terminal = false
		rep.Detail = fmt.Sprintf("an out-of-process finalizer (pid %d) has not reported yet", st.FinalizerPID)
	case st.FinalizeErr != "":
		rep.Status = StatusFinalizeFailed
		rep.Detail = st.FinalizeErr
	case st.RolledBack:
		rep.Status = StatusRolledBack
		rep.Detail = "the previous binary was restored after repeated crashes"
	case st.Pending():
		rep.Status = StatusProbation
		rep.Terminal = false
		rep.Detail = "on probation until the build proves it can work"
	case st.Healthy && st.Unproven:
		rep.Status = StatusUnproven
		rep.Since = st.HealthyAt
		rep.Detail = "probation expired without the build satisfying the health probe"
	case st.Healthy:
		rep.Status = StatusHealthy
		rep.Since = st.HealthyAt
	default:
		rep.Status = StatusIdle
	}
	return rep, nil
}

// HealthProbe is evidence that the running build actually works. It is
// supplied by the caller because only the caller knows what working means:
// a heartbeat that completed a round trip, a patchstate snapshot that
// published. A nil probe means no evidence is available, and a build with no
// evidence is never declared healthy on the strength of the clock alone.
type HealthProbe func(context.Context) error

// WatchConfig configures Watch. The durations default to the package
// constants; tests set them small.
type WatchConfig struct {
	StateDir      string
	Log           *slog.Logger
	Probe         HealthProbe
	MinProbation  time.Duration
	MaxProbation  time.Duration
	ProbeInterval time.Duration
}

func (c WatchConfig) normalized() WatchConfig {
	if c.MinProbation <= 0 {
		c.MinProbation = MinProbation
	}
	if c.MaxProbation <= 0 {
		c.MaxProbation = MaxProbation
	}
	if c.MaxProbation < c.MinProbation {
		c.MaxProbation = c.MinProbation
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = ProbeInterval
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return c
}

// Watch supervises the probation window of a freshly swapped build. It marks
// the build healthy only once the probe reports evidence of function, and it
// never deletes the previous binary. Run it as a supervised task.
//
// A build that never satisfies the probe stays on probation until
// MaxProbation, at which point the crash counter is disarmed and the outcome
// is recorded as unproven rather than as healthy.
func Watch(ctx context.Context, cfg WatchConfig) error {
	cfg = cfg.normalized()
	t := NewTracker(cfg.StateDir)
	st, err := t.Load()
	if err != nil {
		return err
	}
	if !st.Pending() {
		// No update on probation. Sit until shutdown so the supervisor does
		// not treat an immediate return as a crashed task.
		<-ctx.Done()
		return nil
	}
	if cfg.Probe == nil {
		cfg.Log.Warn("update health probe is not wired, so this build cannot be confirmed by evidence",
			"version", st.PendingVersion, "max_probation", cfg.MaxProbation.String())
	}

	start := st.SwappedAt
	if start.IsZero() {
		start = time.Now()
	}
	if !sleepUntil(ctx, start.Add(cfg.MinProbation)) {
		return nil
	}

	// mark ends probation, reporting whether it took. An outstanding
	// finalizer is the one case where it deliberately does not: the swap has
	// not happened, so there is nothing here to declare good. That is a
	// stalled update to report, not a task failure to restart.
	mark := func(unproven bool) (bool, error) {
		var err error
		if unproven {
			err = t.MarkUnproven()
		} else {
			err = t.MarkHealthy()
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, ErrFinalizePending):
			cfg.Log.Error("the finalizer has not reported, so this host is still running the previous binary",
				"version", st.PendingVersion, "finalizer_pid", st.FinalizerPID)
			return false, nil
		default:
			return false, err
		}
	}

	tick := time.NewTicker(cfg.ProbeInterval)
	defer tick.Stop()
	deadline := start.Add(cfg.MaxProbation)
	for {
		if cfg.Probe != nil {
			perr := cfg.Probe(ctx)
			if perr == nil {
				done, err := mark(false)
				if err != nil {
					return err
				}
				if done {
					cfg.Log.Info("update confirmed healthy",
						"version", st.PendingVersion, "previous", st.PreviousVersion)
				}
				<-ctx.Done()
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			cfg.Log.Warn("update still unproven", "version", st.PendingVersion, "err", perr)
		}
		if !time.Now().Before(deadline) {
			if _, err := mark(true); err != nil {
				return err
			}
			cfg.Log.Error("update probation expired without evidence, the previous binary is still on disk",
				"version", st.PendingVersion, "backup", st.Backup)
			<-ctx.Done()
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// sleepUntil waits for a wall clock instant, reporting false when ctx ended
// first.
func sleepUntil(ctx context.Context, when time.Time) bool {
	d := time.Until(when)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// WatchHealth is the probe-less form of Watch, kept so a caller that has not
// been wired to a health probe yet still compiles. It can never confirm a
// build: it only holds the probation open until MaxProbation, which keeps
// the rollback armed and the backup in place.
func WatchHealth(ctx context.Context, stateDir string, log *slog.Logger) error {
	return Watch(ctx, WatchConfig{StateDir: stateDir, Log: log})
}

// MarkHealthy is the package level form of Tracker.MarkHealthy.
func MarkHealthy(stateDir string) error { return NewTracker(stateDir).MarkHealthy() }

// CheckAndRollback is the package level form of Tracker.CheckAndRollback.
func CheckAndRollback(stateDir string) (rolledBack bool, err error) {
	return NewTracker(stateDir).CheckAndRollback()
}

// Status is the package level form of Tracker.Report.
func Status(stateDir string) (Report, error) { return NewTracker(stateDir).Report() }
