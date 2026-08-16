package patch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
)

// comThread owns one OS thread with an established apartment and runs every
// COM call on it. The apartment setup itself is platform specific and lives
// in com_windows.go; everything here is the plumbing that decides what
// crosses back to the caller, which is where the interesting failure was.
type comThread struct {
	reqs    chan func()
	ready   chan struct{}
	initErr error
}

// startCOMThread pins a thread, runs enter to establish the apartment, and
// serves requests until the process ends. The thread is never unlocked: when
// the goroutine exits the thread dies with it, which is what we want for a
// thread carrying apartment state.
func startCOMThread(enter func() error, leave func()) *comThread {
	t := &comThread{reqs: make(chan func()), ready: make(chan struct{})}
	go t.loop(enter, leave)
	<-t.ready
	return t
}

func (t *comThread) loop(enter func() error, leave func()) {
	runtime.LockOSThread()
	if err := enter(); err != nil {
		t.initErr = err
	}
	close(t.ready)
	if t.initErr != nil {
		return // comDo checks initErr and never sends, so nothing blocks
	}
	if leave != nil {
		defer leave()
	}
	for fn := range t.reqs {
		fn()
	}
}

// comState says what happened to a call, which is not the same question as
// what it returned.
type comState int

const (
	// comNotStarted: the COM thread never took the call. Nothing ran.
	comNotStarted comState = iota
	// comCompleted: the call ran to completion and the value is real.
	comCompleted
	// comAbandoned: the caller stopped waiting and the call is STILL RUNNING
	// on the COM thread. Nothing it touches is ours to read.
	comAbandoned
)

type comResult[T any] struct {
	val T
	err error
}

// comDo runs fn on the COM thread and waits for it.
//
// ctx bounds the WAIT, not the call: an in-flight COM call cannot be
// interrupted, so a cancelled context abandons the result while the call
// runs to completion. That is deliberate. Killing a Windows Update install
// midway is far worse than waiting for it.
//
// What is NOT optional is that the value and the error come back through the
// same channel, by value. Handing a pointer into the closure and reading it
// after an abandoned wait is a data race with a live writer, and when the
// shared state is a map (which InstallResult.Failed is) the Go runtime
// answers a concurrent read and write with a throw that recover cannot
// catch. Cancelling one patch job would kill the whole agent. An abandoned
// wait has to abandon everything, not just the error.
func comDo[T any](ctx context.Context, t *comThread, fn func() (T, error)) (T, comState, error) {
	var zero T
	if t.initErr != nil {
		return zero, comNotStarted, fmt.Errorf("com initialize: %w", t.initErr)
	}
	// Checked before the send, not only inside the select. A select whose
	// cases are both ready picks at random, so an already-cancelled context
	// racing an idle COM thread would start an install roughly half the time
	// after the caller had already given up on it.
	if err := ctx.Err(); err != nil {
		return zero, comNotStarted, err
	}
	// Buffered by one: the COM thread must be able to deliver and move on
	// even when nobody is waiting any more.
	done := make(chan comResult[T], 1)
	select {
	case t.reqs <- func() { done <- callRecovered(fn) }:
	case <-ctx.Done():
		return zero, comNotStarted, ctx.Err()
	}
	select {
	case out := <-done:
		return out.val, comCompleted, out.err
	case <-ctx.Done():
		// Cancellation and completion can land together. A result that
		// genuinely arrived is worth more than a deadline, so take it.
		select {
		case out := <-done:
			return out.val, comCompleted, out.err
		default:
		}
		return zero, comAbandoned, ctx.Err()
	}
}

// callRecovered turns a panic from the COM layer into an error. go-ole
// panics on some malformed variants, and one bad update in a search result
// must not take the agent down.
func callRecovered[T any](fn func() (T, error)) (out comResult[T]) {
	defer func() {
		if r := recover(); r != nil {
			out.err = fmt.Errorf("com call panicked: %v\n%s", r, debug.Stack())
		}
	}()
	out.val, out.err = fn()
	return out
}

// ErrInstallOutcomeUnknown is reported per update when the caller stopped
// waiting for an install that cannot be interrupted. It is deliberately not
// a failure: the install is probably still running, and "failed" would
// invite a retry that installs the same updates a second time.
var ErrInstallOutcomeUnknown = errors.New(
	"install outcome unknown: the agent stopped waiting (cancelled or past its deadline) " +
		"while the install was still running on the update thread; it may still complete")

// abandonedInstallResult is the honest answer for an install we walked away
// from: every requested update has an unknown outcome, and none of the real
// result is safe to read.
func abandonedInstallResult(ids []string) InstallResult {
	res := newInstallResult()
	for _, id := range ids {
		res.fail(id, ErrInstallOutcomeUnknown)
	}
	return res
}

// installViaCOM runs one install on the COM thread with the two guarantees
// the obvious version does not give.
//
// The result crosses the abandonment boundary by value, so a cancelled job
// cannot leave the caller iterating a map the COM thread is still writing.
//
// And gate is held until the COM side actually finishes, not until the
// caller gives up on it. Releasing the gate early would let a second install
// be admitted while the first is still running inside Windows Update, which
// is the exact contention the gate exists to prevent.
func installViaCOM(ctx context.Context, t *comThread, gate *installGate, ids []string, fn func(res *InstallResult) error) (InstallResult, error) {
	if !gate.acquire() {
		return newInstallResult(), ErrBusy
	}
	res, state, err := comDo(ctx, t, func() (InstallResult, error) {
		// Released on the COM thread, when the install is genuinely over.
		defer gate.release()
		local := newInstallResult()
		return local, fn(&local)
	})
	switch state {
	case comNotStarted:
		// The closure never ran, so nothing will ever release the gate.
		gate.release()
		return newInstallResult(), err
	case comAbandoned:
		return abandonedInstallResult(ids), err
	default:
		return res, err
	}
}
