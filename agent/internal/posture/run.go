package posture

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// errToolMissing means the tool this check needs is not installed.
//
// Distinguished from a tool that ran and failed, because they mean different
// things to an operator: one is a machine that never had the tool, the other is
// a machine where it broke.
var errToolMissing = errors.New("tool not installed")

// output runs a command and returns its stdout, trimmed.
//
// Every posture check that shells out goes through here so the failure modes
// are handled identically. Getting this wrong once, in one check, is how a
// missing binary on one platform turns into a fleet reported as non-compliant.
func output(ctx context.Context, name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errToolMissing
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Some tools report a meaningful state through a non-zero exit and
		// still write usable output, so the caller gets both.
		return strings.TrimSpace(stdout.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// unknown builds the result for a check that could not determine an answer.
//
// A helper rather than a literal at each site, because the one thing every
// check must get right is that "I could not tell" is not "it failed", and a
// helper is harder to get wrong than a remembered convention.
func unknown(check, detail string) Result {
	return Result{Check: check, Status: Unknown, Detail: detail}
}

func notApplicable(check, detail string) Result {
	return Result{Check: check, Status: NotApplicable, Detail: detail}
}

func pass(check, detail string, evidence map[string]string) Result {
	return Result{Check: check, Status: Pass, Detail: detail, Evidence: evidence}
}

func fail(check, detail string, evidence map[string]string) Result {
	return Result{Check: check, Status: Fail, Detail: detail, Evidence: evidence}
}
