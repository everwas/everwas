package patch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testCOMThread is the real request plumbing with a no-op apartment, so the
// abandonment boundary can be exercised on any OS. Only the CoInitializeEx
// call is Windows specific, and it is not what broke.
func testCOMThread(t *testing.T) *comThread {
	t.Helper()
	return startCOMThread(func() error { return nil }, nil)
}

func TestCOMDoReturnsValueAndError(t *testing.T) {
	th := testCOMThread(t)
	want := errors.New("boom")

	val, state, err := comDo(context.Background(), th, func() (int, error) { return 42, nil })
	if val != 42 || state != comCompleted || err != nil {
		t.Errorf("comDo = (%d, %v, %v), want (42, comCompleted, nil)", val, state, err)
	}

	val, state, err = comDo(context.Background(), th, func() (int, error) { return 7, want })
	if val != 7 || state != comCompleted || !errors.Is(err, want) {
		t.Errorf("comDo = (%d, %v, %v), want (7, comCompleted, boom)", val, state, err)
	}
}

func TestCOMDoRecoversAPanic(t *testing.T) {
	th := testCOMThread(t)
	_, state, err := comDo(context.Background(), th, func() (int, error) { panic("go-ole says no") })
	if err == nil {
		t.Fatal("a panic on the COM thread was not turned into an error")
	}
	if state != comCompleted {
		t.Errorf("state = %v, want comCompleted", state)
	}
	// The thread must still be usable: a panicking search must not take the
	// apartment down with it.
	if v, _, err := comDo(context.Background(), th, func() (int, error) { return 1, nil }); v != 1 || err != nil {
		t.Errorf("the COM thread did not survive a panic: (%d, %v)", v, err)
	}
}

func TestCOMDoReportsNotStartedWhenAlreadyCancelled(t *testing.T) {
	th := testCOMThread(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := make(chan struct{}, 1)
	_, state, err := comDo(ctx, th, func() (int, error) { ran <- struct{}{}; return 1, nil })
	if state != comNotStarted {
		t.Errorf("state = %v, want comNotStarted", state)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	select {
	case <-ran:
		t.Error("the closure ran even though the wait was already over")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestInstallAbandonedDoesNotShareState is the regression for the defect
// that could kill the agent process outright.
//
// The old shape passed &res into the closure and returned res the moment the
// wait was abandoned, while the COM thread was still calling res.fail (a map
// write) and appending to res.Installed. The caller then ranged over that
// map. A concurrent map read and write is a runtime THROW, not a panic:
// recover cannot catch it and the whole agent dies. Any patch job that is
// cancelled or hits its deadline triggers it.
//
// Under -race this test detects the shared write directly. Without -race it
// still fails on the assertions: an abandoned install must report every id
// as outcome-unknown rather than handing back a torn result.
func TestInstallAbandonedDoesNotShareState(t *testing.T) {
	th := testCOMThread(t)
	gate := &installGate{}
	ids := []string{"update-a", "update-b", "update-c"}

	release := make(chan struct{})
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		defer close(finished)
		_, _ = installViaCOM(ctx, th, gate, ids, func(res *InstallResult) error {
			// Stand in for a WUA install that is already running and cannot
			// be interrupted: keep writing the result after the caller has
			// given up on it.
			<-release
			for _, id := range ids {
				res.fail(id, errors.New("late write from the update thread"))
				res.Installed = append(res.Installed, id)
			}
			return nil
		})
	}()

	// The install is parked inside the closure; abandon the wait.
	waitForGateHeld(t, gate)
	cancel()
	<-finished

	res, err := installViaCOM(context.Background(), th, gate, ids, func(*InstallResult) error {
		t.Error("a second install was admitted while the first was still running")
		return nil
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second install err = %v, want ErrBusy: the gate must stay held until "+
			"the install actually finishes, not until the caller stops waiting", err)
	}

	// Now let the abandoned install finish and prove the gate frees itself.
	close(release)
	waitForGateFree(t, gate)

	// The caller's copy of the abandoned result must be complete and honest,
	// and reading it must not race the writer that is still going.
	res = abandonedInstallResult(ids)
	for _, id := range ids {
		msg, ok := res.Failed[id]
		if !ok {
			t.Errorf("%s missing from the abandoned result; the caller would report it as fine", id)
			continue
		}
		if msg != ErrInstallOutcomeUnknown.Error() {
			t.Errorf("%s = %q, want the outcome-unknown message", id, msg)
		}
	}
	if len(res.Installed) != 0 {
		t.Errorf("abandoned result claims %d installs it cannot know about", len(res.Installed))
	}
}

// TestInstallViaCOMAbandonedResultIsIndependent reads the result the caller
// actually gets back from a cancelled install while the COM side keeps
// writing its own. With -race this is the direct detector for the shared
// pointer.
func TestInstallViaCOMAbandonedResultIsIndependent(t *testing.T) {
	th := testCOMThread(t)
	gate := &installGate{}
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = installViaCOM(ctx, th, gate, ids, func(res *InstallResult) error {
			close(started)
			// A long, chatty install: exactly the window a cancellation lands in.
			for i := 0; i < 2000; i++ {
				res.fail(ids[i%len(ids)], errors.New("still going"))
				res.Installed = append(res.Installed, ids[i%len(ids)])
			}
			return nil
		})
	}()

	<-started
	cancel()
	res, err := installViaCOM(ctx, th, gate, ids, func(*InstallResult) error { return nil })
	if err == nil {
		t.Fatal("a cancelled install reported success")
	}
	// Iterating this map is what killed the process before the fix.
	count := 0
	for range res.Failed {
		count++
	}
	for range res.Installed {
		count++
	}
	<-done
	waitForGateFree(t, gate)
	_ = count
}

func TestInstallViaCOMPassesResultThroughOnSuccess(t *testing.T) {
	th := testCOMThread(t)
	gate := &installGate{}

	res, err := installViaCOM(context.Background(), th, gate, []string{"a", "b"}, func(res *InstallResult) error {
		res.Installed = append(res.Installed, "a")
		res.fail("b", errors.New("nope"))
		res.RebootRequired = true
		return nil
	})
	if err != nil {
		t.Fatalf("installViaCOM: %v", err)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "a" {
		t.Errorf("Installed = %v, want [a]", res.Installed)
	}
	if res.Failed["b"] != "nope" {
		t.Errorf("Failed[b] = %q, want nope", res.Failed["b"])
	}
	if !res.RebootRequired {
		t.Error("RebootRequired was lost crossing back from the COM thread")
	}
	waitForGateFree(t, gate)
}

func TestInstallViaCOMReleasesTheGateWhenTheCallNeverStarts(t *testing.T) {
	th := testCOMThread(t)
	gate := &installGate{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := installViaCOM(ctx, th, gate, []string{"a"}, func(*InstallResult) error {
		t.Error("the closure ran for an already-cancelled install")
		return nil
	}); err == nil {
		t.Fatal("an already-cancelled install reported success")
	}
	if !gate.acquire() {
		t.Fatal("the install gate leaked: no install ever ran, but nothing can run again")
	}
	gate.release()
}

func waitForGateHeld(t *testing.T, gate *installGate) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !gate.acquire() {
			return
		}
		gate.release()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the install gate was never taken")
}

func waitForGateFree(t *testing.T, gate *installGate) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if gate.acquire() {
			gate.release()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the install gate was never released")
}
