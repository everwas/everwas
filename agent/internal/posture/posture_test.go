package posture

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

// stub is a check with a fixed outcome.
type stub struct {
	name   string
	result Result
	delay  time.Duration
	panics bool
}

func (s stub) Name() string { return s.name }

func (s stub) Run(ctx context.Context) Result {
	if s.panics {
		panic("this check is broken")
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return unknown(s.name, "timed out")
		}
	}
	return s.result
}

func TestOnlyPassAndFailAreVerdicts(t *testing.T) {
	// The property the whole package is built around. Anything that branches
	// on `status != Pass` silently turns "we could not tell" into "it failed",
	// and posture gates network access, so that mistake takes healthy machines
	// off the network in bulk.
	for _, tc := range []struct {
		status Status
		want   bool
	}{
		{Pass, true},
		{Fail, true},
		{NotApplicable, false},
		{Unknown, false},
	} {
		if got := tc.status.Assessed(); got != tc.want {
			t.Errorf("%s.Assessed() = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestABrokenCheckCannotTakeDownTheAgent(t *testing.T) {
	// A posture check is the last thing that should be able to kill the
	// process that would tell anyone about it. A panic becomes Unknown, and
	// the checks either side of it still report.
	results := Run(context.Background(), []Check{
		stub{name: "a", result: pass("a", "fine", nil)},
		stub{name: "b", panics: true},
		stub{name: "c", result: fail("c", "bad", nil)},
	}, quiet())

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: a panicking check lost its neighbours", len(results))
	}
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Check] = r
	}
	if byName["b"].Status != Unknown {
		t.Errorf("a panicking check reported %s, want unknown", byName["b"].Status)
	}
	if byName["a"].Status != Pass || byName["c"].Status != Fail {
		t.Error("a panicking check disturbed the results either side of it")
	}
}

func TestAPanickingCheckIsNotReportedAsFailing(t *testing.T) {
	// Specifically NOT Fail. A check whose code is broken says nothing about
	// the machine it was pointed at, and reporting it as a failure would take
	// the whole fleet off the network the day the check is broken, because a
	// broken check is broken everywhere at once.
	results := Run(context.Background(), []Check{stub{name: "b", panics: true}}, quiet())
	if results[0].Status == Fail {
		t.Fatal("a broken check reported the MACHINE as failing, which would fail " +
			"every machine in the fleet simultaneously")
	}
}

func TestResultsAreSortedSoUnchangedPostureLooksUnchanged(t *testing.T) {
	// The output is hashed to decide whether anything changed. Goroutine
	// completion order is not stable, so unsorted results would produce a new
	// "change" on most runs and fill the history with churn that is not
	// change. The same mistake was made and fixed once in network inventory.
	checks := []Check{
		stub{name: "zulu", result: pass("zulu", "", nil), delay: 5 * time.Millisecond},
		stub{name: "alpha", result: pass("alpha", "", nil)},
		stub{name: "mike", result: pass("mike", "", nil), delay: 2 * time.Millisecond},
	}
	for range 5 {
		results := Run(context.Background(), checks, quiet())
		if results[0].Check != "alpha" || results[1].Check != "mike" || results[2].Check != "zulu" {
			t.Fatalf("results are not in name order: %v %v %v",
				results[0].Check, results[1].Check, results[2].Check)
		}
	}
}

func TestAHungCheckDoesNotStallTheOnesBehindIt(t *testing.T) {
	// Checks run concurrently, so one slow tool must not add its time to the
	// total. This runs three checks that each sleep, and asserts the whole
	// batch takes about as long as the slowest rather than their sum.
	slow := 300 * time.Millisecond
	checks := []Check{
		stub{name: "a", result: pass("a", "", nil), delay: slow},
		stub{name: "b", result: pass("b", "", nil), delay: slow},
		stub{name: "c", result: pass("c", "", nil), delay: slow},
	}
	started := time.Now()
	Run(context.Background(), checks, quiet())
	if elapsed := time.Since(started); elapsed > 2*slow {
		t.Errorf("three concurrent %s checks took %s: they ran in series", slow, elapsed)
	}
}

func TestACancelledContextStopsTheChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Must return rather than hang; the machine is shutting down.
	done := make(chan struct{})
	go func() {
		Run(ctx, []Check{stub{name: "a", result: pass("a", "", nil), delay: time.Hour}}, quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run ignored a cancelled context, so shutdown would hang")
	}
}

func TestSummarizeKeepsNotAssessedOutOfTheVerdicts(t *testing.T) {
	s := Summarize([]Result{
		{Check: "a", Status: Pass},
		{Check: "b", Status: Fail},
		{Check: "c", Status: NotApplicable},
		{Check: "d", Status: Unknown},
	})
	if s.AssessedCount != 2 {
		t.Errorf("AssessedCount = %d, want 2", s.AssessedCount)
	}
	if len(s.Failed) != 1 || s.Failed[0] != "b" {
		t.Errorf("Failed = %v, want [b]: something that was not assessed was counted as a failure", s.Failed)
	}
	if len(s.NotAssessed) != 2 {
		t.Errorf("NotAssessed = %v, want both c and d", s.NotAssessed)
	}
}

func TestAnUnnamedResultStillGetsAttributed(t *testing.T) {
	// A check that forgets to set Result.Check would otherwise produce a
	// finding nothing can attribute to anything.
	results := Run(context.Background(),
		[]Check{stub{name: "named", result: Result{Status: Pass}}}, quiet())
	if results[0].Check != "named" {
		t.Errorf("Check = %q, want the check's own name", results[0].Check)
	}
}
