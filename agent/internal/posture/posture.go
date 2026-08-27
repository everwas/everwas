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
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// Category groups checks so a policy can be written against a property rather
// than an enumeration of check names.
//
// The point is survivability: a site policy that gates on `encryption` keeps
// covering the machine when a fifth encryption check is added, where one
// listing disk-encryption, bitlocker, filevault and luks by name silently stops
// covering it. So this is a fact about the check, like its name, and we own the
// assignment for the same reason we own the name.
//
// Deliberately small, and additive-only for exactly the reason check names are:
// a category is something somebody's policy is written against, and renaming one
// breaks that policy silently rather than loudly.
type Category string

const (
	CategoryEncryption Category = "encryption"
	CategoryMalware    Category = "malware"
	CategoryFirewall   Category = "firewall"
	// CategoryNetwork arrives with the 802.1X identity-source check on Windows.
	// A category with no check in it looks covered while gating nothing, so
	// they ship together.
	CategoryNetwork Category = "network"
)

// NotAssessedReason distinguishes the two ways a check produces no verdict.
//
// A REASON rather than a status, deliberately and by agreement with the
// verifier side. Both mean "not assessed" to anything making a decision, and
// keeping the distinction as a separate field means nothing downstream can
// promote one of them to a verdict by matching on the status alone. The
// difference still matters to an operator: not-applicable is permanent and
// expected, undetermined is a collection fault worth fixing.
type NotAssessedReason string

const (
	ReasonNotApplicable NotAssessedReason = "not_applicable"
	ReasonUndetermined  NotAssessedReason = "undetermined"
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
	Check string `json:"check"`

	// Category is what a policy gates on when it does not want to name this
	// check specifically. Populated from the Check, not set per result, so a
	// check cannot report itself under different categories on different runs.
	Category Category `json:"category,omitempty"`

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

// MarshalJSON emits the agreed wire shape: THREE statuses, with the
// not-applicable versus undetermined distinction carried as a reason.
//
// The four-state Status is internal. It was reaching the wire directly, which
// meant a consumer branching on the status string would see values the agreed
// schema does not contain, and the natural handling of an unrecognised status
// is a default branch that treats it as a failure. Shaping it here rather than
// at each call site means nothing can serialise a Result into the internal
// form by accident.
func (r Result) MarshalJSON() ([]byte, error) {
	type wire struct {
		Check    string            `json:"check"`
		Category Category          `json:"category,omitempty"`
		Status   string            `json:"status"`
		Reason   NotAssessedReason `json:"not_assessed_reason,omitempty"`
		Detail   string            `json:"detail,omitempty"`
		Evidence map[string]string `json:"evidence,omitempty"`
		TookMS   int64             `json:"took_ms,omitempty"`
	}
	w := wire{
		Check:    r.Check,
		Category: r.Category,
		Detail:   r.Detail,
		Evidence: r.Evidence,
		TookMS:   r.TookMS,
	}
	switch r.Status {
	case NotApplicable:
		w.Status, w.Reason = "not_assessed", ReasonNotApplicable
	case Unknown:
		w.Status, w.Reason = "not_assessed", ReasonUndetermined
	default:
		w.Status = string(r.Status)
	}
	return json.Marshal(w)
}

// Check is one thing worth knowing about this machine's security state.
//
// Implementations must be safe to run concurrently with each other and must
// respect ctx: they run on a timer on somebody's laptop, not in a batch job.
type Check interface {
	// Name is the stable identifier used in Result.Check.
	Name() string
	// Category groups this check for policy. See Category.
	Category() Category
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
	// Always from the check, never from the result: a check that reported
	// itself under different categories on different runs would break any
	// policy written against the category, intermittently.
	res.Category = c.Category()
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
