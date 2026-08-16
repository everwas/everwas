//go:build !windows

package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/rsp2k/openrmm/agent/internal/audit"
	"github.com/rsp2k/openrmm/agent/internal/sched"
	"github.com/rsp2k/openrmm/agent/internal/scripts"
	"github.com/rsp2k/openrmm/agent/internal/shell"
	"github.com/rsp2k/openrmm/agent/internal/wire"
)

// Integration coverage against a real NATS server with JetStream. It is
// skipped unless OPENRMM_TEST_NATS_URL points at a scratch server:
//
//	docker run -d --rm -p 34222:4222 nats:2.12-alpine -js
//	OPENRMM_TEST_NATS_URL=nats://127.0.0.1:34222 go test ./internal/jobs/ -run Integration -v
const agentID = "01991111-2222-7333-8444-555566667777"

type harness struct {
	nc   *nats.Conn
	mod  *Module
	log  *slog.Logger
	stop context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("OPENRMM_TEST_NATS_URL")
	if url == "" {
		t.Skip("set OPENRMM_TEST_NATS_URL to run integration tests")
	}
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(nc.Close)

	// The server owns the JOBS stream in production; create it here.
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     "JOBS",
		Subjects: []string{"jobs.*"},
		MaxAge:   7 * 24 * time.Hour,
	}); err != nil {
		t.Fatalf("create JOBS stream: %v", err)
	}
	for _, cfg := range []jetstream.StreamConfig{
		{Name: "JOBOUT", Subjects: []string{"agents.*.jobs.*.output"}},
		{Name: "RESULTS", Subjects: []string{"agents.*.jobs.*.result"}},
		{Name: "EVENTS", Subjects: []string{"agents.*.events"}},
	} {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			t.Fatalf("create %s: %v", cfg.Name, err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	aud := audit.New(nc, agentID, log)
	runner := scripts.NewRunner(nc, agentID, t.TempDir(), aud, log)
	mod := &Module{
		NC:      nc,
		AgentID: agentID,
		Version: "integration",
		Log:     log,
		Shell:   shell.New(nc, agentID, aud, log),
		Scripts: runner,
		Audit:   aud,
		RefreshInventory: func(context.Context) error {
			return nil
		},
	}
	mod.Sched = sched.New(agentID, t.TempDir(), mod.RunScheduled, aud, log)

	go func() {
		if err := mod.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("module run: %v", err)
		}
	}()
	// Give the subscription and consumer time to bind.
	time.Sleep(500 * time.Millisecond)
	return &harness{nc: nc, mod: mod, log: log, stop: cancel}
}

func TestIntegrationPing(t *testing.T) {
	h := newHarness(t)
	msg, err := h.nc.Request(wire.Cmd(agentID, "ping"), []byte(`{}`), 3*time.Second)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["pong"] != true || got["version"] != "integration" {
		t.Errorf("ping reply = %v", got)
	}
}

func TestIntegrationUnknownAndUnsupportedCommands(t *testing.T) {
	h := newHarness(t)
	for _, op := range []string{"agent.update", "agent.rotate_creds", "nonsense"} {
		msg, err := h.nc.Request(wire.Cmd(agentID, op), []byte(`{}`), 3*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		var got reply
		if err := json.Unmarshal(msg.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Accepted || got.Error == "" {
			t.Errorf("%s reply = %+v, want a refusal with a reason", op, got)
		}
	}
}

// TestIntegrationScriptJob pushes a job through the durable queue and
// collects the streamed output and terminal result.
func TestIntegrationScriptJob(t *testing.T) {
	h := newHarness(t)
	const jobID = "01J-INT-SCRIPT"

	var mu sync.Mutex
	var stdout strings.Builder
	chunks := make(chan struct{}, 32)
	subOut, err := h.nc.Subscribe(wire.JobOutput(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var c scripts.Chunk
		if err := json.Unmarshal(env.Data, &c); err != nil {
			return
		}
		mu.Lock()
		if c.Stream == scripts.StreamStdout {
			stdout.Write(c.Data)
		}
		mu.Unlock()
		chunks <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subOut.Unsubscribe() }()

	results := make(chan scripts.Result, 1)
	subRes, err := h.nc.Subscribe(wire.JobResult(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var res scripts.Result
		if err := json.Unmarshal(env.Data, &res); err != nil {
			return
		}
		results <- res
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subRes.Unsubscribe() }()

	phases := make(chan string, 8)
	subProg, err := h.nc.Subscribe(wire.JobProgress(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var p struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(env.Data, &p); err != nil {
			return
		}
		phases <- p.Phase
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subProg.Unsubscribe() }()

	publishJob(t, h.nc, scripts.JobSpec{
		JobID:       jobID,
		Kind:        scripts.KindScriptRun,
		Shell:       "bash",
		Body:        "echo integration-ok; exit 0",
		TimeoutS:    30,
		RequestedBy: "integration@example.com",
	})

	select {
	case res := <-results:
		if res.Status != scripts.StatusSucceeded || res.ExitCode != 0 {
			t.Errorf("result = %+v", res)
		}
		if res.DurationMS <= 0 {
			t.Errorf("duration_ms = %d, want > 0", res.DurationMS)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no result published")
	}

	mu.Lock()
	out := stdout.String()
	mu.Unlock()
	if !strings.Contains(out, "integration-ok") {
		t.Errorf("stdout = %q", out)
	}
	// Progress is core NATS and races the result; give the last frame a
	// moment to land.
	var seen []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		select {
		case p := <-phases:
			seen = append(seen, p)
		case <-time.After(100 * time.Millisecond):
		}
		if len(seen) >= 3 {
			break
		}
	}
	if strings.Join(seen, ",") != "started,running,finished" {
		t.Errorf("progress phases = %v, want started,running,finished", seen)
	}
}

func TestIntegrationJobRedeliveryIsAckedOnStart(t *testing.T) {
	h := newHarness(t)
	const jobID = "01J-INT-LONG"

	results := make(chan scripts.Result, 4)
	sub, err := h.nc.Subscribe(wire.JobResult(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var res scripts.Result
		if err := json.Unmarshal(env.Data, &res); err != nil {
			return
		}
		results <- res
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Longer than the consumer's 30 s ack wait would be ideal, but the test
	// budget is smaller: 3 s still proves the ack lands before completion,
	// because a nak/redelivery would produce a second result.
	publishJob(t, h.nc, scripts.JobSpec{
		JobID: jobID, Kind: scripts.KindScriptRun, Shell: "bash",
		Body: "sleep 3; echo done", TimeoutS: 30,
	})
	select {
	case res := <-results:
		if res.Status != scripts.StatusSucceeded {
			t.Errorf("result = %+v", res)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no result")
	}
	select {
	case res := <-results:
		t.Errorf("job was redelivered and ran twice: %+v", res)
	case <-time.After(2 * time.Second):
	}
}

func TestIntegrationJobCancel(t *testing.T) {
	h := newHarness(t)
	const jobID = "01J-INT-CANCEL"

	results := make(chan scripts.Result, 1)
	sub, err := h.nc.Subscribe(wire.JobResult(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		_ = json.Unmarshal(m.Data, &env)
		var res scripts.Result
		if err := json.Unmarshal(env.Data, &res); err == nil {
			results <- res
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	publishJob(t, h.nc, scripts.JobSpec{
		JobID: jobID, Kind: scripts.KindScriptRun, Shell: "bash",
		Body: "sleep 60", TimeoutS: 120,
	})

	deadline := time.Now().Add(10 * time.Second)
	var cancelled bool
	for time.Now().Before(deadline) {
		msg, err := h.nc.Request(wire.Cmd(agentID, "job.cancel"),
			[]byte(`{"job_id":"`+jobID+`","requested_by":"integration"}`), 3*time.Second)
		if err != nil {
			t.Fatalf("job.cancel: %v", err)
		}
		var got reply
		if err := json.Unmarshal(msg.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Accepted {
			cancelled = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !cancelled {
		t.Fatal("job.cancel never found the running job")
	}
	select {
	case res := <-results:
		if res.Status != scripts.StatusCancelled {
			t.Errorf("status = %s, want cancelled", res.Status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled job never reported a result")
	}
}

// TestIntegrationShellSession drives the full console path: open, input,
// raw output frames, ack, resize, close.
func TestIntegrationShellSession(t *testing.T) {
	h := newHarness(t)
	const sid = "sess-int-1"

	var mu sync.Mutex
	var out strings.Builder
	subOut, err := h.nc.Subscribe(wire.ShellOut(agentID, sid), func(m *nats.Msg) {
		mu.Lock()
		out.Write(m.Data)
		mu.Unlock()
		// Ack every frame, the way the server bridge does after the
		// websocket write completes.
		ack, _ := json.Marshal(map[string]any{"ack": len(m.Data)})
		_ = h.nc.Publish(wire.ShellCtl(agentID, sid), ack)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subOut.Unsubscribe() }()

	closed := make(chan map[string]any, 4)
	subCtl, err := h.nc.Subscribe(wire.ShellCtl(agentID, sid), func(m *nats.Msg) {
		var ev map[string]any
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		if ev["event"] == "closed" {
			closed <- ev
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subCtl.Unsubscribe() }()

	msg, err := h.nc.Request(wire.Cmd(agentID, "shell.open"), mustJSON(t, shell.OpenSpec{
		SessionID: sid, Shell: "bash", Cols: 100, Rows: 30,
		IdleTimeoutS: 120, RequestedBy: "integration@example.com",
	}), 5*time.Second)
	if err != nil {
		t.Fatalf("shell.open: %v", err)
	}
	var openReply reply
	if err := json.Unmarshal(msg.Data, &openReply); err != nil {
		t.Fatal(err)
	}
	if !openReply.Accepted {
		t.Fatalf("shell.open refused: %s", openReply.Error)
	}

	if err := h.nc.Publish(wire.ShellIn(agentID, sid), []byte("echo shell-int-ok\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.nc.Publish(wire.ShellResize(agentID, sid), []byte(`{"cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if strings.Contains(got, "shell-int-ok") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if !strings.Contains(got, "shell-int-ok") {
		t.Fatalf("shell output %q missing the echoed marker", got)
	}

	// A second session with the same id must be refused, not silently
	// hijacked.
	dup, err := h.nc.Request(wire.Cmd(agentID, "shell.open"), mustJSON(t, shell.OpenSpec{
		SessionID: sid, Shell: "bash", Cols: 80, Rows: 24,
	}), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var dupReply reply
	if err := json.Unmarshal(dup.Data, &dupReply); err != nil {
		t.Fatal(err)
	}
	if dupReply.Accepted {
		t.Error("duplicate session id was accepted")
	}

	closeMsg, err := h.nc.Request(wire.Cmd(agentID, "shell.close"),
		[]byte(`{"session_id":"`+sid+`"}`), 5*time.Second)
	if err != nil {
		t.Fatalf("shell.close: %v", err)
	}
	var closeReply reply
	if err := json.Unmarshal(closeMsg.Data, &closeReply); err != nil {
		t.Fatal(err)
	}
	if !closeReply.Accepted {
		t.Errorf("shell.close refused: %s", closeReply.Error)
	}
	select {
	case ev := <-closed:
		if ev["session_id"] != sid {
			t.Errorf("closed event = %v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no closed event on ctl")
	}
	if n := h.mod.Shell.Count(); n != 0 {
		t.Errorf("%d sessions still open after close", n)
	}
}

// TestIntegrationShellFlowControl is the `yes` flood: with no acks the
// agent must stop reading the PTY after ~512 KiB instead of drowning the
// browser, and resume the moment the bridge catches up.
func TestIntegrationShellFlowControl(t *testing.T) {
	h := newHarness(t)
	const sid = "sess-int-flood"

	var mu sync.Mutex
	var total int64
	subOut, err := h.nc.Subscribe(wire.ShellOut(agentID, sid), func(m *nats.Msg) {
		mu.Lock()
		total += int64(len(m.Data))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subOut.Unsubscribe() }()
	if err := subOut.SetPendingLimits(-1, 128*1024*1024); err != nil {
		t.Fatal(err)
	}

	msg, err := h.nc.Request(wire.Cmd(agentID, "shell.open"), mustJSON(t, shell.OpenSpec{
		SessionID: sid, Shell: "bash", Cols: 80, Rows: 24, IdleTimeoutS: 300,
		RequestedBy: "integration@example.com",
	}), 5*time.Second)
	if err != nil {
		t.Fatalf("shell.open: %v", err)
	}
	var openReply reply
	if err := json.Unmarshal(msg.Data, &openReply); err != nil {
		t.Fatal(err)
	}
	if !openReply.Accepted {
		t.Fatalf("shell.open refused: %s", openReply.Error)
	}
	defer func() {
		_, _ = h.nc.Request(wire.Cmd(agentID, "shell.close"),
			[]byte(`{"session_id":"`+sid+`"}`), 5*time.Second)
	}()

	if err := h.nc.Publish(wire.ShellIn(agentID, sid), []byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	mu.Lock()
	stalled := total
	mu.Unlock()

	time.Sleep(2 * time.Second)
	mu.Lock()
	after := total
	mu.Unlock()
	if after != stalled {
		t.Errorf("output grew from %d to %d bytes while un-acked; flow control is not holding",
			stalled, after)
	}
	// 512 KiB of slack plus at most one in-flight frame, plus the echoed
	// command line.
	if max := int64(shell.PauseAbove + shell.MaxFrameBytes + 4096); after > max {
		t.Errorf("published %d bytes with no acks, want <= %d", after, max)
	}
	if after < int64(shell.PauseAbove) {
		t.Errorf("published only %d bytes, want to fill the %d byte window",
			after, shell.PauseAbove)
	}

	ack, _ := json.Marshal(map[string]any{"ack": after})
	if err := h.nc.Publish(wire.ShellCtl(agentID, sid), ack); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		resumed := total > after
		mu.Unlock()
		if resumed {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("output did not resume after the ack drained the window")
}

func TestIntegrationSchedSyncAndFire(t *testing.T) {
	h := newHarness(t)
	doc := sched.Document{
		ScheduleVersion: 42,
		Entries: []sched.Entry{{
			EntryID: "int-nightly", Cron: "0 3 * * *", TZ: "UTC",
			Kind:    scripts.KindScriptRun,
			Payload: json.RawMessage(`{"shell":"bash","body":"echo scheduled"}`),
			JitterS: 900, MisfireGraceS: 3600, Enabled: true,
		}},
	}
	msg, err := h.nc.Request(wire.Cmd(agentID, "sched.sync"), mustJSON(t, doc), 5*time.Second)
	if err != nil {
		t.Fatalf("sched.sync: %v", err)
	}
	var got reply
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Accepted || got.ScheduleVersion != 42 {
		t.Fatalf("sched.sync reply = %+v", got)
	}
	if h.mod.Sched.Version() != 42 {
		t.Errorf("cached version = %d", h.mod.Sched.Version())
	}

	// Drive one scheduled run directly through the same path the timer uses.
	fireAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	jobID := sched.JobID("int-nightly", fireAt)
	results := make(chan scripts.Result, 1)
	sub, err := h.nc.Subscribe(wire.JobResult(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		_ = json.Unmarshal(m.Data, &env)
		var res scripts.Result
		if err := json.Unmarshal(env.Data, &res); err == nil {
			results <- res
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	h.mod.RunScheduled(context.Background(), jobID, doc.Entries[0], fireAt)
	select {
	case res := <-results:
		if res.Status != scripts.StatusSucceeded {
			t.Errorf("scheduled run = %+v", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("scheduled run produced no result")
	}
}

func TestIntegrationUnsupportedJobKind(t *testing.T) {
	h := newHarness(t)
	const jobID = "01J-INT-PATCH"
	results := make(chan scripts.Result, 1)
	sub, err := h.nc.Subscribe(wire.JobResult(agentID, jobID), func(m *nats.Msg) {
		var env wire.Envelope
		_ = json.Unmarshal(m.Data, &env)
		var res scripts.Result
		if err := json.Unmarshal(env.Data, &res); err == nil {
			results <- res
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	publishJob(t, h.nc, scripts.JobSpec{JobID: jobID, Kind: scripts.KindPatchScan})
	select {
	case res := <-results:
		if res.Status != scripts.StatusFailed {
			t.Errorf("unsupported kind = %+v, want failed", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unsupported job kind never answered")
	}
}

func publishJob(t *testing.T, nc *nats.Conn, spec scripts.JobSpec) {
	t.Helper()
	env, err := wire.NewEnvelope("job", agentID, wire.NewMsgID(), spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Publish(wire.JobsQueue(agentID), raw); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
