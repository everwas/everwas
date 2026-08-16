package patch

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// HRESULTs that CoInitializeEx returns for "already initialised". Neither
// is a failure for us: the thread still has a usable apartment.
const (
	hrSFalse          = 0x00000001
	hrRPCEChangedMode = 0x80010106
)

// comThread owns one OS thread with an initialised STA apartment and runs
// every COM call on it.
//
// This is not optional decoration. COM apartment state is per THREAD, and
// Go moves goroutines between threads freely: a goroutine that calls
// CoInitializeEx and then makes a call after a scheduling point can find
// itself on a thread with no apartment at all, which surfaces as
// CO_E_NOTINITIALIZED from somewhere deep inside wuapi.dll. Locking one
// thread and funnelling every request through it is the only arrangement
// that holds.
type comThread struct {
	reqs    chan func()
	ready   chan struct{}
	initErr error
}

func newCOMThread() *comThread {
	t := &comThread{reqs: make(chan func()), ready: make(chan struct{})}
	go t.loop()
	<-t.ready
	return t
}

func (t *comThread) loop() {
	// Never unlocked: when this goroutine exits, the thread dies with it,
	// which is what we want for a thread carrying apartment state.
	runtime.LockOSThread()

	// COINIT_APARTMENTTHREADED, not MULTITHREADED: the Windows Update Agent
	// objects are apartment threaded, and an MTA caller pays for a proxy on
	// every property read of a several-thousand-item search result.
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if !alreadyInitialized(err) {
			t.initErr = err
		}
	}
	close(t.ready)
	if t.initErr != nil {
		return // do() checks initErr and never sends, so nothing blocks
	}
	defer ole.CoUninitialize()
	for fn := range t.reqs {
		fn()
	}
}

// alreadyInitialized reports whether a CoInitializeEx error just means the
// thread already had an apartment.
func alreadyInitialized(err error) bool {
	oleErr, ok := err.(*ole.OleError)
	if !ok {
		return false
	}
	code := uint32(oleErr.Code())
	return code == hrSFalse || code == hrRPCEChangedMode
}

// do runs fn on the COM thread and waits for it.
//
// ctx bounds the WAIT, not the call: an in-flight COM call cannot be
// interrupted, so a cancelled context abandons the result while the call
// runs to completion on the COM thread. That is deliberate. Killing a
// Windows Update install midway is far worse than waiting for it.
func (t *comThread) do(ctx context.Context, fn func() error) error {
	if t.initErr != nil {
		return fmt.Errorf("com initialize: %w", t.initErr)
	}
	done := make(chan error, 1)
	select {
	case t.reqs <- func() { done <- callRecovered(fn) }:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// callRecovered turns a panic from the COM layer into an error. go-ole
// panics on some malformed variants, and one bad update in a search result
// must not take the agent down.
func callRecovered(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("com call panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return fn()
}

// createObject instantiates a COM object by ProgID and returns its
// IDispatch. The caller releases it.
func createObject(progID string) (*ole.IDispatch, error) {
	unknown, err := oleutil.CreateObject(progID)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", progID, err)
	}
	defer unknown.Release()
	disp, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil, fmt.Errorf("%s IDispatch: %w", progID, err)
	}
	return disp, nil
}

// callDispatch invokes a method that returns an object.
func callDispatch(d *ole.IDispatch, name string, args ...any) (*ole.IDispatch, error) {
	v, err := oleutil.CallMethod(d, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	disp := v.ToIDispatch()
	if disp == nil {
		return nil, fmt.Errorf("%s did not return an object", name)
	}
	return disp, nil
}

// propDispatch reads a property that holds an object. The caller releases
// the returned dispatch.
func propDispatch(d *ole.IDispatch, name string, args ...any) (*ole.IDispatch, error) {
	v, err := oleutil.GetProperty(d, name, args...)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", name, err)
	}
	disp := v.ToIDispatch()
	if disp == nil {
		return nil, fmt.Errorf("property %s is not an object", name)
	}
	return disp, nil
}

// propString reads a string property, or "" when it is absent. Missing
// properties are normal across Windows releases and are not errors.
func propString(d *ole.IDispatch, name string, args ...any) string {
	v, err := oleutil.GetProperty(d, name, args...)
	if err != nil {
		return ""
	}
	defer func() { _ = v.Clear() }()
	if v.VT == ole.VT_BSTR {
		return v.ToString()
	}
	switch val := v.Value().(type) {
	case nil:
		return ""
	case string:
		return val
	default:
		return fmt.Sprint(val)
	}
}

// propInt64 reads a numeric property, or 0 when it is absent.
func propInt64(d *ole.IDispatch, name string, args ...any) int64 {
	v, err := oleutil.GetProperty(d, name, args...)
	if err != nil {
		return 0
	}
	defer func() { _ = v.Clear() }()
	return toInt64(v.Value())
}

// propBool reads a boolean property, or false when it is absent.
func propBool(d *ole.IDispatch, name string, args ...any) bool {
	v, err := oleutil.GetProperty(d, name, args...)
	if err != nil {
		return false
	}
	defer func() { _ = v.Clear() }()
	if b, ok := v.Value().(bool); ok {
		return b
	}
	return toInt64(v.Value()) != 0
}

// toInt64 normalises whatever numeric type a VARIANT decoded into.
func toInt64(val any) int64 {
	switch n := val.(type) {
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// collectStrings reads an IStringCollection (KBArticleIDs and friends).
func collectStrings(d *ole.IDispatch, name string) []string {
	coll, err := propDispatch(d, name)
	if err != nil {
		return nil
	}
	defer coll.Release()
	count := propInt64(coll, "Count")
	out := make([]string, 0, count)
	for i := int64(0); i < count; i++ {
		out = append(out, propString(coll, "Item", int(i)))
	}
	return out
}

// collectCategoryNames reads an ICategoryCollection down to its names.
func collectCategoryNames(d *ole.IDispatch) []string {
	coll, err := propDispatch(d, "Categories")
	if err != nil {
		return nil
	}
	defer coll.Release()
	count := propInt64(coll, "Count")
	out := make([]string, 0, count)
	for i := int64(0); i < count; i++ {
		item, err := propDispatch(coll, "Item", int(i))
		if err != nil {
			continue
		}
		out = append(out, propString(item, "Name"))
		item.Release()
	}
	return out
}
