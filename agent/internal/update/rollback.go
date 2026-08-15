package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Rollback policy.
const (
	// HealthyAfter is how long the new build must stay connected before we
	// throw away the binary it replaced.
	HealthyAfter = 60 * time.Second
	// CrashWindow is how long after a swap we keep counting restarts.
	CrashWindow = 2 * time.Minute
	// CrashLimit is the number of extra starts inside CrashWindow tolerated
	// before the previous binary is restored. Start one is the normal
	// post-update launch; starts two and three are the two crashes.
	CrashLimit = 2

	stateFileName    = "update-state.json"
	healthMarkerName = "health-ok"
	maxStartsTracked = 16
)

// ErrState wraps failures to read or write the update state file.
var ErrState = errors.New("update: state file")

// State is the on-disk record of an in-flight update. It exists only between
// a swap and the moment the new build reports healthy (or gets rolled back).
type State struct {
	PendingVersion  string      `json:"pending_version,omitempty"`
	PreviousVersion string      `json:"previous_version,omitempty"`
	Target          string      `json:"target,omitempty"`
	Backup          string      `json:"backup,omitempty"`
	SwappedAt       time.Time   `json:"swapped_at,omitempty"`
	Starts          []time.Time `json:"starts,omitempty"`
	Healthy         bool        `json:"healthy"`
	RolledBack      bool        `json:"rolled_back,omitempty"`
}

// Pending reports whether an update is still on probation.
func (s State) Pending() bool { return s.PendingVersion != "" && !s.Healthy }

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

func (t *Tracker) statePath() string  { return filepath.Join(t.StateDir, stateFileName) }
func (t *Tracker) markerPath() string { return filepath.Join(t.StateDir, healthMarkerName) }

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

// Clear removes the state file and the health marker.
func (t *Tracker) Clear() error {
	if err := os.Remove(t.statePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: remove: %v", ErrState, err)
	}
	if err := os.Remove(t.markerPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: remove marker: %v", ErrState, err)
	}
	return nil
}

// BeginUpdate records an update as in flight. Call it immediately before the
// swap so that a crash between the two leaves a harmless orphan record
// instead of an unrecoverable one.
func (t *Tracker) BeginUpdate(newVersion, prevVersion, target, backup string) error {
	_ = os.Remove(t.markerPath())
	return t.Save(State{
		PendingVersion:  newVersion,
		PreviousVersion: prevVersion,
		Target:          target,
		Backup:          backup,
		SwappedAt:       t.now(),
	})
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

// MarkHealthy declares the running build good: the health marker is written,
// the previous binary is deleted, and the update state is cleared. It is safe
// to call when no update is pending.
func (t *Tracker) MarkHealthy() error {
	st, err := t.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir: %v", ErrState, err)
	}
	marker := fmt.Sprintf("version=%s\nhealthy_at=%s\n", st.PendingVersion, t.now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(t.markerPath(), []byte(marker), 0o600); err != nil {
		return fmt.Errorf("%w: write marker: %v", ErrState, err)
	}
	if !st.Pending() {
		return nil
	}
	if st.Backup != "" {
		// A backup we cannot delete is clutter, not a failure: Windows
		// scanners hold handles on recently written files all the time.
		_ = os.Remove(st.Backup)
	}
	return t.finishHealthy(st)
}

func (t *Tracker) finishHealthy(st State) error {
	st.Healthy = true
	st.Starts = nil
	return t.Save(st)
}

// shouldRollback is the whole rollback decision, kept pure so it can be
// tested without touching a filesystem or a clock.
func shouldRollback(st State, now time.Time) bool {
	if !st.Pending() || st.Backup == "" || st.Target == "" {
		return false
	}
	if st.RolledBack {
		return false
	}
	n := 0
	for _, s := range st.Starts {
		if !s.After(now) && now.Sub(s) <= CrashWindow {
			n++
		}
	}
	return n > CrashLimit
}

// CheckAndRollback runs at startup, before anything else. It records this
// launch and, if the new build has crash looped, restores the previous binary
// and reports true. A true return means the caller should exit non-zero so
// the service manager starts the restored binary.
func (t *Tracker) CheckAndRollback() (bool, error) {
	st, err := t.RecordStart()
	if err != nil {
		return false, err
	}
	if !shouldRollback(st, t.now()) {
		return false, nil
	}
	if err := RestoreBackup(st.Target); err != nil {
		// Nothing to restore, or the restore failed. Either way, stop
		// counting: repeating a rollback that cannot work just adds noise.
		st.RolledBack = true
		st.PendingVersion = ""
		if saveErr := t.Save(st); saveErr != nil {
			return false, saveErr
		}
		return false, err
	}
	st.RolledBack = true
	st.Healthy = false
	st.PendingVersion = ""
	st.Starts = nil
	if err := t.Save(st); err != nil {
		return true, err
	}
	return true, nil
}

// WatchHealth blocks for HealthyAfter and then marks the running build
// healthy. Run it as a supervised task once NATS is connected: if the process
// dies first, the marker is never written and the crash counter keeps
// climbing, which is exactly what drives the rollback.
func WatchHealth(ctx context.Context, stateDir string, log *slog.Logger) error {
	t := NewTracker(stateDir)
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
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(HealthyAfter):
	}
	if err := t.MarkHealthy(); err != nil {
		return err
	}
	if log != nil {
		log.Info("update confirmed healthy", "version", st.PendingVersion, "previous", st.PreviousVersion)
	}
	<-ctx.Done()
	return nil
}

// MarkHealthy is the package level form of Tracker.MarkHealthy.
func MarkHealthy(stateDir string) error { return NewTracker(stateDir).MarkHealthy() }

// CheckAndRollback is the package level form of Tracker.CheckAndRollback.
func CheckAndRollback(stateDir string) (rolledBack bool, err error) {
	return NewTracker(stateDir).CheckAndRollback()
}
