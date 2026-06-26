package integrity

import (
	"context"
	"fmt"
)

// Family groups checks by what kind of guarantee they provide. It is
// descriptive (metrics, docs, level presets) and does not affect execution.
type Family int

const (
	// FamilyCommitment — recompute a cryptographic commitment (block hash,
	// transactions/receipts root, logs bloom) and compare it to the data.
	FamilyCommitment Family = iota
	// FamilyAuthenticity — per-item cryptographic authenticity (sender
	// recovery, transaction-hash recompute).
	FamilyAuthenticity
	// FamilyStructural — cross-reference invariants over a block/aggregate
	// (index contiguity, embedded-block-identity, counts).
	FamilyStructural
	// FamilyShape — cheap shape/sanity checks (field lengths, index magnitude,
	// bloom emptiness, schema conformance).
	FamilyShape
	// FamilyContinuity — cross-block / reorg-awareness (parent-hash linkage,
	// fork detection). Stateful; reserved for later phases.
	FamilyContinuity
)

// FailureClass decides how a violation should be treated. It does not change
// the returned error type today (always a content-validation error), but it
// lets callers distinguish a transient reorg-window mismatch from a provable
// corruption when deciding whether to retry vs hard-fail.
type FailureClass int

const (
	// Deterministic — provable from committed data; cannot be a transient race.
	Deterministic FailureClass = iota
	// ReorgSensitive — can be a transient artifact of the reorg window across
	// load-balanced backends; a caller may prefer to retry before giving up.
	ReorgSensitive
)

// Violation is a check's verdict that the response is invalid. Reason is a
// human-readable explanation; the engine prefixes it with the check id.
type Violation struct {
	Reason string
}

// failf builds a Violation with a formatted reason.
func failf(format string, args ...any) *Violation {
	return &Violation{Reason: fmt.Sprintf(format, args...)}
}

// Check is one self-contained integrity validation.
type Check struct {
	// ID is the stable identifier used in config, headers, and metrics.
	ID string
	// Family is the descriptive grouping (see Family).
	Family Family
	// Class is how a violation should be treated (see FailureClass).
	Class FailureClass
	// Methods are the lowercased JSON-RPC methods this check applies to.
	Methods []string
	// Run inspects the decoded response and returns a Violation, or nil when
	// the response satisfies this check (including when the data the check
	// needs is absent — an absent field is not a violation).
	Run func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation
}

// registry maps a lowercased method to the checks that apply to it. Checks
// self-register from their init() functions, mirroring the consensus rules
// pattern, so adding a check is a single localized edit.
var registry = map[string][]*Check{}

// allChecks is the flat registration order, used for introspection/tests.
var allChecks []*Check

func register(c *Check) {
	allChecks = append(allChecks, c)
	for _, m := range c.Methods {
		registry[m] = append(registry[m], c)
	}
}

// checksFor returns the checks registered for a lowercased method.
func checksFor(method string) []*Check {
	return registry[method]
}
