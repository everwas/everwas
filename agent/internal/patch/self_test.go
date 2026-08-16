package patch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPackageNameOf(t *testing.T) {
	cases := map[string]string{
		"curl=8.5.0-2":                       "curl",
		"openrmm-agent=1.2.3":                "openrmm-agent",
		"NetworkManager.x86_64=1:1.44.2-1":   "NetworkManager",
		"openrmm-agent.x86_64=1:1.2.3-1.el9": "openrmm-agent",
		"macOS Sequoia 15.5-24F74":           "macOS Sequoia 15.5-24F74",
		"":                                   "",
	}
	for id, want := range cases {
		if got := packageNameOf(id); got != want {
			t.Errorf("packageNameOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestRefuseSelfPackages is the H6 self-inflicted case: the agent installing
// its own package runs its own postinst, which restarts the service, which
// tears down the cgroup containing the package manager doing the installing.
// Across a patch group that happens on every host at once.
func TestRefuseSelfPackages(t *testing.T) {
	res := newInstallResult()
	got := refuseSelfPackages([]string{
		"curl=8.5.0-2",
		"openrmm-agent=1.2.3",
		"OpenRMM-Agent=1.2.3",
		"openrmm-agent.x86_64=1:1.2.3-1.el9",
		"tzdata=2024a-1",
	}, &res)

	want := []string{"curl=8.5.0-2", "tzdata=2024a-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
	for _, id := range []string{"openrmm-agent=1.2.3", "OpenRMM-Agent=1.2.3", "openrmm-agent.x86_64=1:1.2.3-1.el9"} {
		if res.Failed[id] == "" {
			t.Errorf("%s was dropped without a reason; the operator has to be told why", id)
		}
	}
}

// TestCheckInstallDeadline is the H7 regression: the job layer capped the
// server's timeout at 24h but had no floor, so timeout_s=60 handed dpkg a
// deadline it could never meet.
func TestCheckInstallDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := checkInstallDeadline(ctx)
	if !errors.Is(err, ErrTimeoutTooShort) {
		t.Errorf("a one minute install deadline was accepted: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel2()
	if err := checkInstallDeadline(ctx2); err != nil {
		t.Errorf("a four hour deadline must be fine: %v", err)
	}

	if err := checkInstallDeadline(context.Background()); err != nil {
		t.Errorf("no deadline at all must be fine: %v", err)
	}
}

// TestPrepareInstallFailsEveryIDOnAShortDeadline checks the refusal is loud:
// every requested id carries the reason, so the job result says what happened
// rather than reporting an empty success.
func TestPrepareInstallFailsEveryIDOnAShortDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := newInstallResult()
	ids, err := prepareInstall(ctx, []string{"curl=8.5.0-2", "tzdata=2024a-1"}, &res)
	if !errors.Is(err, ErrTimeoutTooShort) {
		t.Fatalf("err = %v, want ErrTimeoutTooShort", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want nothing handed to a package manager", ids)
	}
	if len(res.Failed) != 2 {
		t.Errorf("failed = %v, want every requested id accounted for", res.Failed)
	}
}
