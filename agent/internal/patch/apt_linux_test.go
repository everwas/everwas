package patch

import (
	"errors"
	"slices"
	"testing"
)

// TestAttributeAptResultAsksDpkgOnFailure is the H8 regression. apt exits
// non-zero when any package in the transaction fails, and the verification
// against dpkg used to run only on the success path, so one held back package
// made the agent report every requested update as failed. dnf already gets
// this right; this makes apt match.
func TestAttributeAptResultAsksDpkgOnFailure(t *testing.T) {
	want := map[string]string{
		"curl":    "8.5.0-2",
		"openssl": "3.0.13-1",
		"tzdata":  "2024a-1",
	}
	idByName := map[string]string{
		"curl":    "curl=8.5.0-2",
		"openssl": "openssl=3.0.13-1",
		"tzdata":  "tzdata=2024a-1",
	}
	// dpkg says two of the three landed, which is what a partially failed
	// apt transaction actually looks like.
	installed := map[string]string{
		"curl":    "8.5.0-2",
		"openssl": "3.0.13-1",
	}

	res := newInstallResult()
	attributeAptResult(want, idByName, installed, errors.New("apt-get install exited 100"), "E: Sub-process returned an error code", &res)

	slices.Sort(res.Installed)
	wantInstalled := []string{"curl=8.5.0-2", "openssl=3.0.13-1"}
	if !slices.Equal(res.Installed, wantInstalled) {
		t.Errorf("installed = %v, want %v: dpkg has these on disk", res.Installed, wantInstalled)
	}
	if len(res.Failed) != 1 || res.Failed["tzdata=2024a-1"] == "" {
		t.Errorf("failed = %v, want only the update dpkg does not have", res.Failed)
	}
}

func TestAttributeAptResultReportsAWrongVersion(t *testing.T) {
	want := map[string]string{"curl": "8.5.0-2"}
	idByName := map[string]string{"curl": "curl=8.5.0-2"}
	installed := map[string]string{"curl": "8.4.0-1"}

	res := newInstallResult()
	attributeAptResult(want, idByName, installed, nil, "", &res)

	if len(res.Installed) != 0 {
		t.Errorf("installed = %v, want none: the version on disk is not the one approved", res.Installed)
	}
	if got := res.Failed["curl=8.5.0-2"]; got == "" {
		t.Error("a held back version must be reported as failed")
	}
}

func TestAttributeAptResultCleanSuccess(t *testing.T) {
	want := map[string]string{"curl": "8.5.0-2"}
	idByName := map[string]string{"curl": "curl=8.5.0-2"}

	res := newInstallResult()
	attributeAptResult(want, idByName, map[string]string{"curl": "8.5.0-2"}, nil, "", &res)

	if len(res.Installed) != 1 || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want one installed and nothing failed", res)
	}
}
