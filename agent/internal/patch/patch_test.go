package patch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDedupeIDs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "duplicates collapse in place", in: []string{"a", "b", "a", "c", "b"},
			want: []string{"a", "b", "c"}},
		{name: "empties dropped", in: []string{"", "a", ""}, want: []string{"a"}},
		{name: "nil", in: nil, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dedupeIDs(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dedupeIDs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestInstallGate(t *testing.T) {
	var g installGate
	if !g.acquire() {
		t.Fatal("first acquire must succeed")
	}
	if g.acquire() {
		t.Error("second acquire must fail while the gate is held")
	}
	g.release()
	if !g.acquire() {
		t.Error("acquire must succeed again after release")
	}
}

func TestInstallResultFail(t *testing.T) {
	res := newInstallResult()
	res.fail("a", errors.New("first"))
	res.fail("a", errors.New("second"))
	if res.Failed["a"] != "first" {
		t.Errorf("failure = %q, want the first one recorded", res.Failed["a"])
	}
	// A zero-value result must still take a failure without panicking.
	var bare InstallResult
	bare.fail("b", errors.New("boom"))
	if bare.Failed["b"] != "boom" {
		t.Errorf("failure on a zero-value result = %q", bare.Failed["b"])
	}
}

func TestNewInstallResultIsJSONStable(t *testing.T) {
	res := newInstallResult()
	if res.Installed == nil || res.Failed == nil {
		t.Error("installed and failed must marshal as [] and {}, never null")
	}
}

func TestPctOf(t *testing.T) {
	tests := []struct {
		seen, total, want int
	}{
		{0, 10, 10},
		{5, 10, 50},
		{10, 10, 90},
		{20, 10, 90}, // more lines than packages must not exceed the band
		{1, 0, 10},
		{0, 0, 10},
	}
	for _, tc := range tests {
		if got := pctOf(tc.seen, tc.total); got != tc.want {
			t.Errorf("pctOf(%d, %d) = %d, want %d", tc.seen, tc.total, got, tc.want)
		}
	}
}

func TestEmitProgressTolerationOfNil(t *testing.T) {
	emitProgress(nil, InstallProgress{Phase: PhaseInstall})

	var got InstallProgress
	emitProgress(func(p InstallProgress) { got = p }, InstallProgress{Phase: PhaseDownload, Pct: 42})
	if got.Phase != PhaseDownload || got.Pct != 42 {
		t.Errorf("progress = %+v", got)
	}
}

func TestScrubEnv(t *testing.T) {
	host := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"AWS_SECRET_ACCESS_KEY=leakme",
		"OPENRMM_AGENT_SECRET=leakmetoo",
		"https_proxy=http://proxy.example:3128",
		"malformed",
	}
	got := scrubEnv(host, map[string]string{"DEBIAN_FRONTEND": "noninteractive", "BAD=NAME": "x"})
	want := []string{
		"DEBIAN_FRONTEND=noninteractive",
		"HOME=/root",
		"PATH=/usr/bin",
		"https_proxy=http://proxy.example:3128",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %v\nwant %v", got, want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "leakme") {
			t.Errorf("a secret survived the scrub: %q", kv)
		}
	}
}

func TestNoninteractiveEnv(t *testing.T) {
	env := noninteractiveEnv(map[string]string{"LC_ALL": "en_US.UTF-8"})
	if env["DEBIAN_FRONTEND"] != "noninteractive" {
		t.Error("DEBIAN_FRONTEND must be set so apt never prompts")
	}
	if env["LC_ALL"] != "en_US.UTF-8" {
		t.Error("caller extras must win over the defaults")
	}
}

func TestRunCmdCapturesExitCode(t *testing.T) {
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	// A non-zero exit is data, not an error: dnf uses 100 to mean "updates
	// are available" and needs-restarting uses 1 to mean "reboot needed".
	res := runCmd(context.Background(), execOptions{}, "sh", "-c", "echo out; echo err >&2; exit 100")
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ExitCode != 100 {
		t.Errorf("exit code = %d, want 100", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("stdout = %q, stderr = %q", res.Stdout, res.Stderr)
	}
}

func TestRunCmdScrubsEnvironment(t *testing.T) {
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	t.Setenv("OPENRMM_TEST_SECRET", "leakme")
	res := runCmd(context.Background(), execOptions{}, "sh", "-c", "echo \"${OPENRMM_TEST_SECRET:-absent}\"")
	if strings.TrimSpace(res.Stdout) != "absent" {
		t.Errorf("a non-allowlisted variable reached the command: %q", res.Stdout)
	}
}

func TestRunCmdStreamsLines(t *testing.T) {
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	var lines []string
	res := runCmd(context.Background(), execOptions{
		OnLine: func(l string) { lines = append(lines, l) },
	}, "sh", "-c", "echo one; echo two; echo three")
	if !res.ok() {
		t.Fatalf("command failed: %+v", res)
	}
	if !reflect.DeepEqual(lines, []string{"one", "two", "three"}) {
		t.Errorf("streamed lines = %v", lines)
	}
	if res.Stdout != "one\ntwo\nthree\n" {
		t.Errorf("accumulated stdout = %q", res.Stdout)
	}
}

func TestRunCmdCancellation(t *testing.T) {
	if !have("sh") {
		t.Skip("no sh on this host")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := runCmd(ctx, execOptions{Timeout: 30 * time.Second}, "sh", "-c", "sleep 30")
	if res.Err == nil {
		t.Fatal("a cancelled command must report an error")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("cancellation took %s, the process was not killed", time.Since(start))
	}
}

func TestRunCmdMissingBinary(t *testing.T) {
	res := runCmd(context.Background(), execOptions{}, "openrmm-no-such-binary-xyz")
	if res.Err == nil {
		t.Fatal("a missing binary must be an error, not an exit code")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
}

func TestCmdResultTail(t *testing.T) {
	res := cmdResult{Stdout: "one\ntwo\nthree\nfour\nfive\n"}
	if got := res.tail(2); got != "four\nfive" {
		t.Errorf("tail(2) = %q", got)
	}
	if got := res.tail(99); !strings.HasPrefix(got, "one") {
		t.Errorf("tail beyond the output = %q", got)
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("an uncancelled sleep must report completion")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("a cancelled sleep must report interruption immediately")
	}
}

func TestSeverityRank(t *testing.T) {
	order := []string{SeverityUnknown, SeverityLow, SeverityModerate, SeverityImportant, SeverityCritical}
	for i := 1; i < len(order); i++ {
		if severityRank(order[i]) <= severityRank(order[i-1]) {
			t.Errorf("%s must rank above %s", order[i], order[i-1])
		}
	}
}

// TestDetectReportsSomething keeps Detect honest on whatever platform the
// suite runs on: either a backend or ErrUnsupported, never both nil.
func TestDetectReportsSomething(t *testing.T) {
	m, err := Detect()
	switch {
	case err != nil && !errors.Is(err, ErrUnsupported):
		t.Fatalf("unexpected detect error: %v", err)
	case err != nil && m != nil:
		t.Error("an error must not come with a manager")
	case err == nil && m == nil:
		t.Fatal("no error and no manager")
	case err == nil && m.Kind() == "":
		t.Error("a manager must name its backend")
	}
}

func TestWithLogger(t *testing.T) {
	m := &aptManagerForTest{}
	if got := WithLogger(m, nil); got != Manager(m) {
		t.Error("WithLogger must return the manager it was given")
	}
}

// aptManagerForTest is a stand-in Manager so WithLogger can be exercised on
// every platform, not just the one whose backend happens to compile.
type aptManagerForTest struct{ loggerHolder }

func (m *aptManagerForTest) Kind() string { return "test" }
func (m *aptManagerForTest) Scan(context.Context) ([]Update, error) {
	return nil, nil
}
func (m *aptManagerForTest) Install(context.Context, []string, func(InstallProgress)) (InstallResult, error) {
	return newInstallResult(), nil
}
func (m *aptManagerForTest) RebootRequired(context.Context) (bool, error) { return false, nil }

func TestLoggerHolderNeverReturnsNil(t *testing.T) {
	var h loggerHolder
	if h.logger() == nil {
		t.Fatal("logger() must never be nil")
	}
	h.logger().Info("this must not panic")
}
