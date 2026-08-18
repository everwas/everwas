package inventory

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/everwas/everwas/agent/internal/patch"
)

// The rule these tests pin, stated once here and applied to every collector:
//
// An empty result set is a CLAIM. "No packages installed", "no services on
// this host", "nobody is logged in", "no patches pending" are all assertions
// about the machine, and the server believes them completely: a snapshot is
// treated as the full truth for its kind, so anything missing from it is
// recorded as having been removed.
//
// A collector that could not look must therefore say so, and the only way to
// say so is to return an error, which makes RefreshNow skip the publish and
// leaves the server believing whatever it last knew.
//
// This was written down in logins.go and then violated in the two files beside
// it within the hour, so the fix is not a comment. It is signatures that cannot
// express the mistake: every enumerator returns ([]T, error), and the empty
// slice is only reachable on a successful look.

func failing(err error) runner {
	return func(context.Context, string, ...string) (string, error) { return "", err }
}

func fixed(out string) runner {
	return func(context.Context, string, ...string) (string, error) { return out, nil }
}

func TestRunReportsFailureRatherThanEmptyOutput(t *testing.T) {
	// The root of the whole class: run() used to swallow every error and
	// return "", which every caller then parsed into an empty list.
	out, err := run(context.Background(), "everwas-no-such-binary-anywhere")
	if err == nil {
		t.Fatalf("running a nonexistent binary reported success, output %q", out)
	}
}

func TestServicesFailRatherThanReportingNoServices(t *testing.T) {
	boom := errors.New("systemctl: connection to bus failed")
	_, err := collectServicesWith(context.Background(), failing(boom))
	if err == nil {
		t.Fatal("a failed systemctl enumeration reported an empty service list as fact")
	}
	if !strings.Contains(err.Error(), "bus failed") {
		t.Errorf("error lost the cause: %v", err)
	}
}

func TestServicesReportEmptyOnlyWhenTheLookSucceeded(t *testing.T) {
	// A host that genuinely runs no services is a real state and must still
	// publish, or a real answer becomes indistinguishable from a failure.
	got, err := collectServicesWith(context.Background(), fixed(""))
	if err != nil {
		t.Fatalf("a successful enumeration of nothing should publish: %v", err)
	}
	if snap, ok := got.(servicesSnapshot); !ok || snap.Services == nil {
		t.Fatalf("want an empty non-nil service list, got %#v", got)
	}
}

func TestPackagesFailRatherThanReportingNoSoftware(t *testing.T) {
	boom := errors.New("dpkg-query: timed out waiting for the frontend lock")
	_, err := collectSoftwareWith(context.Background(), failing(boom))
	if err == nil {
		t.Fatal("a failed package enumeration reported an empty package list as fact")
	}
}

func TestPackagesReportEmptyOnlyWhenTheLookSucceeded(t *testing.T) {
	got, err := collectSoftwareWith(context.Background(), fixed("bash\t5.2-15\n"))
	if err != nil {
		t.Fatalf("a successful enumeration should publish: %v", err)
	}
	snap, ok := got.(softwareSnapshot)
	if !ok || len(snap.Packages) != 1 || snap.Packages[0].Name != "bash" {
		t.Fatalf("want one parsed package, got %#v", got)
	}
}

// A platform with no implementation is a third state, distinct from both. It is
// not a failure to report and not an empty machine: we have no collector. It
// must not publish either, because "no software" is still a claim.
func TestAnUnimplementedPlatformIsNeitherEmptyNorAFailureToReport(t *testing.T) {
	if !errors.Is(errNoCollector, errNoCollector) {
		t.Fatal("errNoCollector must be comparable with errors.Is")
	}
	// RefreshNow must not count it as a failed kind, or every macOS agent
	// warns twice per cycle forever about a platform gap it cannot fix.
	if kindFailed(errNoCollector) {
		t.Error("an unimplemented platform should not be reported as a failed kind")
	}
	if !kindFailed(errors.New("dpkg exploded")) {
		t.Error("a real collection failure must be reported as a failed kind")
	}
}

// --- patchstate: the same rule, on the collector with the widest blast radius

type failingScanner struct{ err error }

func (f failingScanner) Kind() string { return "wua" }
func (f failingScanner) Scan(context.Context) ([]patch.Update, error) {
	return nil, f.err
}
func (f failingScanner) Install(context.Context, []string, func(patch.InstallProgress)) (patch.InstallResult, error) {
	return patch.InstallResult{}, f.err
}
func (f failingScanner) RebootRequired(context.Context) (bool, error) { return false, f.err }

func TestAFailedPatchScanPublishesNothing(t *testing.T) {
	published := 0
	c := &PatchCollector{
		log:       slog.New(slog.DiscardHandler),
		publishFn: func(PatchState) error { published++; return nil },
	}
	// Pre-seed the detection cache so no real backend is probed.
	c.detected, c.mgr = true, failingScanner{err: errors.New("wua: search timed out")}

	if _, err := c.RefreshNow(context.Background()); err == nil {
		t.Fatal("a failed scan reported success")
	}
	// The empty snapshot a failed scan produces is byte-identical to a
	// fully-patched host, and the server would retire every pending patch.
	if published != 0 {
		t.Errorf("published %d snapshots after a failed scan, want 0", published)
	}
}

func TestAnUnsupportedBackendStillPublishes(t *testing.T) {
	published := 0
	c := &PatchCollector{
		log:       slog.New(slog.DiscardHandler),
		publishFn: func(PatchState) error { published++; return nil },
	}
	c.detected, c.detErr = true, patch.ErrUnsupported

	if _, err := c.RefreshNow(context.Background()); err != nil {
		t.Fatalf("an unsupported backend is an answer, not a failure: %v", err)
	}
	// "This host has no patch backend" is a true statement about the host and
	// the console needs it, or a container looks like an agent that stopped
	// reporting.
	if published != 1 {
		t.Errorf("published %d snapshots, want 1", published)
	}
}
