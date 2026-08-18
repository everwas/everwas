package jobs

import (
	"testing"

	"github.com/everwas/everwas/agent/internal/scripts"
)

// entry_id is the ONLY thing that lets the server attribute a scheduled run.
//
// The server never queued a scheduled job, so there is no ScriptRun row to
// match the job id against. _adopt_scheduled_run creates one from entry_id, and
// with entry_id absent it returns None and the result is dropped with a
// "result for unknown run" warning. The run leaves no record anywhere: no row,
// no output, no failure.
//
// It was set in exactly one place, scripts.Runner.Run, which is the happy path.
// Every other terminal publisher built a bare Result: the panic handler, the
// unsupported-kind handler, inventory refresh, patch, update, and the drain
// path that reports jobs the agent could not finish. So a nightly script that
// panicked, or was interrupted by a service restart, vanished silently: exactly
// the failure _adopt_scheduled_run was written to fix, still open on every path
// except the one that already worked.
//
// These tests pin the spec-carrying API rather than each call site, because
// enumerating call sites is what failed.

func specWithEntry(kind string) scripts.JobSpec {
	return scripts.JobSpec{
		JobID:   "01a00000-0000-7000-8000-00000000cafe",
		EntryID: "nightly-backup",
		Kind:    kind,
	}
}

func TestAPanickingScheduledJobStillCarriesItsEntryID(t *testing.T) {
	m, published := moduleWithCapture()
	spec := specWithEntry("script.run")

	m.jobPanicked(spec, "boom", []byte("stack"))

	res := published.lastResult(t)
	if res.EntryID != spec.EntryID {
		t.Errorf("entry_id = %q, want %q: the server drops a scheduled result "+
			"without it, so this nightly run leaves no record at all", res.EntryID, spec.EntryID)
	}
	if res.Status != scripts.StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
}

func TestAnInterruptedScheduledJobStillCarriesItsEntryID(t *testing.T) {
	m, published := moduleWithCapture()
	spec := specWithEntry("script.run")

	// The agent is stopping mid-job: a service restart, a self-update, a
	// reboot. drainJobs publishes a terminal result so the server is not left
	// waiting forever, and for a scheduled job that result must be attributable
	// or it is the same as publishing nothing.
	m.trackJob(spec, func() {})
	m.reportInterruptedSpec(spec)

	res := published.lastResult(t)
	if res.EntryID != spec.EntryID {
		t.Errorf("entry_id = %q, want %q", res.EntryID, spec.EntryID)
	}
	if res.Status != scripts.StatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Status)
	}
}

func TestAnUnsupportedScheduledKindStillCarriesItsEntryID(t *testing.T) {
	m, published := moduleWithCapture()
	spec := specWithEntry("some.future.kind")

	m.unsupportedJob(spec, nil)

	res := published.lastResult(t)
	if res.EntryID != spec.EntryID {
		t.Errorf("entry_id = %q, want %q", res.EntryID, spec.EntryID)
	}
}

func TestStderrChunksAlsoCarryTheEntryID(t *testing.T) {
	// Output is adopted by the same lookup as the result, so a chunk without
	// entry_id is dropped too and the operator loses the reason alongside the
	// record.
	m, published := moduleWithCapture()
	spec := specWithEntry("script.run")

	m.jobPanicked(spec, "boom", []byte("stack"))

	if !published.anyChunkHasEntry(spec.EntryID) {
		t.Errorf("no output chunk carried entry_id %q; chunks: %v",
			spec.EntryID, published.chunkEntries())
	}
}

func TestAnInteractiveJobHasNoEntryID(t *testing.T) {
	// The inverse must hold: a run-now job is not scheduled, and inventing an
	// entry id would attach it to a schedule that did not ask for it.
	m, published := moduleWithCapture()
	spec := scripts.JobSpec{JobID: "01a00000-0000-7000-8000-0000000000ff", Kind: "script.run"}

	m.jobPanicked(spec, "boom", []byte("stack"))

	if res := published.lastResult(t); res.EntryID != "" {
		t.Errorf("entry_id = %q, want empty for an interactive job", res.EntryID)
	}
}
