// Package posture assesses the security state of the machine the agent runs on.
//
// Each thing worth knowing is a Check, and adding one is a file plus a line in
// the platform's list. That modularity is the point: the set of checks WILL
// grow, and the design has to make a check added in six months cost nothing to
// the ones already there.
//
// The property everything else is built around: a check that could not run must
// never read as a check that failed. Posture gates network access, so the cost
// of the two mistakes is wildly asymmetric. A missed failure leaves a
// non-compliant machine on the network, which is roughly where it already was.
// A false failure takes a healthy machine off the network, and does it to every
// machine that shares the cause, which is how a bad antivirus query at 09:00
// becomes a site-wide outage at 09:01.
//
// So there are four outcomes, not two, and only two of them are verdicts.
package posture

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// Status is the outcome of one check.
type Status string

const (
	// Pass and Fail are the only two that are verdicts about the machine.
	Pass Status = "pass"
	Fail Status = "fail"

	// NotApplicable: this check does not mean anything here, and never will.
	// BitLocker on a Linux server. Permanent and expected, not a gap.
	NotApplicable Status = "not_applicable"

	// Unknown: the check applies but could not be determined. A tool was
	// missing, a command failed, a permission was denied. Worth an operator's
	// attention as a COLLECTION problem, and never evidence of non-compliance.
	Unknown Status = "unknown"
)

// Assessed reports whether this status is a verdict about the machine.
//
// Downstream consumers that gate access must branch on this rather than on
// `!= Pass`, which silently turns "we could not tell" into "it failed".
func (s Status) Assessed() bool { return s == Pass || s == Fail }

// Result is one check's finding.
type Result struct {
	// Check is the stable identifier, e.g. "disk-encryption". It appears in
	// stored history and in anything downstream, so it is renamed at the cost
	// of breaking every record that came before.
	Check  string `json:"check"`
	Status Status `json:"status"`

	// Detail is for a human reading a console: why this status, in a sentence.
	Detail string `json:"detail,omitempty"`

	// Evidence is the specifics a machine can act on, e.g. which volumes are
	// unencrypted. Deliberately not free text.
	Evidence map[string]string `json:"evidence,omitempty"`

	// Took is how long the check ran, so a check that is quietly becoming
	// expensive is visible before it starts timing out.
	//
	// Carried on the wire as an explicit millisecond count rather than by
	// tagging the Duration directly: a time.Duration marshals to JSON as its
	// integer NANOSECONDS, so a field tagged "took_ms" holding a Duration is
	// wrong by a factor of a million and looks entirely plausible. Observed as
	// a 4.4ms check reporting 4368661, which reads as either microseconds or
	// an absurdly slow check depending on what the reader assumes.
	Took   time.Duration `json:"-"`
	TookMS int64         `json:"took_ms,omitempty"`
}

// Check is one thing worth knowing about this machine's security state.
//
// Implementations must be safe to run concurrently with each other and must
// respect ctx: they run on a timer on somebody's laptop, not in a batch job.
type Check interface {
	// Name is the stable identifier used in Result.Check.
	Name() string
	// Run assesses the machine. It should return Unknown rather than an error
	// for anything it could not determine; an error return is reserved for
	// nothing at all, and exists only so implementations need not swallow one
	// silently to satisfy the interface.
	Run(ctx context.Context) Result
}

// checkTimeout bounds a single check.
//
// Generous, because some of these shell out to platform tools that are slow on
// a cold cache, and tight enough that a hung tool cannot stall the collection
// behind it. A check that exceeds it is Unknown, which is the honest answer:
// we do not know, because it did not finish.
const checkTimeout = 30 * time.Second

// Run assesses every check and returns the results, sorted by name.
//
// Sorted because the output is hashed to decide whether the machine's posture
// changed, and map or goroutine ordering would otherwise produce a "change" on
// every run and fill the history with churn that is not change. The same
// mistake was already made and fixed once in the network inventory collector.
//
// Every check is isolated: its own timeout, and a recovered panic becomes
// Unknown rather than taking down the agent. A posture check is the last thing
// that should be able to kill the process that would tell anyone about it.
func Run(ctx context.Context, checks []Check, log *slog.Logger) []Result {
	if log == nil {
		// A nil logger here would panic inside the panic handler, which is the
		// one place that must not fail.
		log = slog.Default()
	}
	results := make([]Result, len(checks))
	var wg sync.WaitGroup

	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runOne(ctx, c, log)
		}()
	}
	wg.Wait()

	sort.Slice(results, func(a, b int) bool { return results[a].Check < results[b].Check })
	return results
}

func runOne(ctx context.Context, c Check, log *slog.Logger) (res Result) {
	name := c.Name()
	started := time.Now()

	defer func() {
		if r := recover(); r != nil {
			log.Error("posture check panicked", "check", name, "panic", r,
				"stack", string(debug.Stack()))
			res = Result{
				Check:  name,
				Status: Unknown,
				Detail: "the check failed to run",
			}
		}
		res.Took = time.Since(started)
		res.TookMS = res.Took.Milliseconds()
	}()

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	res = c.Run(ctx)
	// A check that forgot to name itself would otherwise produce a result
	// nothing can attribute.
	if res.Check == "" {
		res.Check = name
	}
	if ctx.Err() != nil && !res.Status.Assessed() {
		res.Detail = fmt.Sprintf("%s (timed out after %s)", res.Detail, checkTimeout)
	}
	return res
}

// Summary is the rollup, and it deliberately does not produce a single boolean.
//
// "Compliant: true/false" cannot represent a machine where two checks passed,
// one failed and one could not run, and any collapse of that into a boolean has
// to decide what Unknown means. Downstream that decision belongs to whoever
// carries the consequence of being wrong, not to the collector.
type Summary struct {
	Passed        []string `json:"passed,omitempty"`
	Failed        []string `json:"failed,omitempty"`
	NotAssessed   []string `json:"not_assessed,omitempty"`
	AssessedCount int      `json:"assessed_count"`
}

// Summarize groups results by outcome.
//
// NotApplicable and Unknown are both reported as not assessed, because that is
// what they mean to anyone acting on this. The distinction between them is kept
// in the individual Result, where an operator can see that a check is missing a
// tool rather than being irrelevant here.
func Summarize(results []Result) Summary {
	var s Summary
	for _, r := range results {
		switch r.Status {
		case Pass:
			s.Passed = append(s.Passed, r.Check)
			s.AssessedCount++
		case Fail:
			s.Failed = append(s.Failed, r.Check)
			s.AssessedCount++
		default:
			s.NotAssessed = append(s.NotAssessed, r.Check)
		}
	}
	return s
}
