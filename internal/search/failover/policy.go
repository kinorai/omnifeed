package failover

import (
	"context"

	"github.com/kinorai/omnifeed/internal/domain"
)

// Trigger names why the backup was asked.
type Trigger string

const (
	// TriggerError means the primary failed (pacing refused it, it timed out, the
	// upstream answered an error status, ...).
	TriggerError Trigger = "error"
	// TriggerEmpty means the primary succeeded with no results — on a pool of
	// engines that block silently, the shape of an outage.
	TriggerEmpty Trigger = "empty"
	// TriggerThin means the primary returned fewer results than Policy.MinResults.
	TriggerThin Trigger = "thin"
	// TriggerDegraded means the primary succeeded without a source it relies on
	// (domain.SearchReport.Degraded).
	TriggerDegraded Trigger = "degraded"
)

// Action says what the composite does with the primary's answer.
type Action int

const (
	// Keep returns the primary's answer as is; the backup is not asked.
	Keep Action = iota
	// Replace answers with the backup alone: the primary has nothing worth
	// keeping.
	Replace
	// Merge answers with both, interleaved: the primary's results are real but
	// weaker than usual, and the backup tops them up.
	Merge
)

// Decision is the policy's verdict on one primary answer.
type Decision struct {
	Action  Action
	Trigger Trigger
}

// Policy decides, from the primary's answer alone, whether the backup is
// worth asking. It is pure — no I/O, no clock — so every rule is a table test.
type Policy struct {
	// MinResults is the result count below which a successful search counts as
	// thin and is topped up. 0 disables the thin trigger.
	MinResults int
}

// Decide returns the action for the primary's (report, err).
//
// A caller that has gone away is never failed over: the backup would answer
// nobody, and spend its own quota doing it.
func (p Policy) Decide(ctx context.Context, report domain.SearchReport, err error) Decision {
	if ctx.Err() != nil {
		return Decision{Action: Keep}
	}
	switch n := len(report.Results); {
	case err != nil:
		return Decision{Action: Replace, Trigger: TriggerError}
	case n == 0:
		return Decision{Action: Replace, Trigger: TriggerEmpty}
	case n < p.MinResults:
		return Decision{Action: Merge, Trigger: TriggerThin}
	case report.Degraded():
		return Decision{Action: Merge, Trigger: TriggerDegraded}
	default:
		return Decision{Action: Keep}
	}
}
