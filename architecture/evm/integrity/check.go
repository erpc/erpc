package integrity

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
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

// String is the label used in logs/archives.
func (c FailureClass) String() string {
	if c == Deterministic {
		return "deterministic"
	}
	return "reorg-sensitive"
}

// Violation is a check's verdict that the response is invalid. Reason is a
// human-readable explanation; the engine prefixes it with the check id.
type Violation struct {
	Reason string
	// DisputedPin (>0) names the block number whose CACHED pin the violation is
	// anchored to. A stale pin after a routine reorg looks identical to
	// corruption, so before acting on such a violation the engine re-confirms
	// the pin against a fresh canonical fetch (PinReconfirmer) and re-runs the
	// check — only a mismatch that survives the fresh pin is genuine.
	DisputedPin int64
}

// Skipped is the sentinel a check returns when it could not perform its
// verification at all — missing wiring (no history/resolver), cold cache,
// canonical unavailable, or data the check does not fully model. It is not a
// violation and never affects the verdict; the engine records it as outcome
// "skip" so that "pass" means an actual verification happened ("N verified,
// 0 mismatches") rather than folding no-ops into passes.
var Skipped = &Violation{Reason: "skipped: check could not evaluate this response"}

// failf builds a Violation with a formatted reason.
func failf(format string, args ...any) *Violation {
	return &Violation{Reason: fmt.Sprintf(format, args...)}
}

// disputes marks the violation as anchored to the cached pin for `number`,
// making it eligible for corroborate-before-verdict (see Violation.DisputedPin).
func (v *Violation) disputes(number int64) *Violation {
	v.DisputedPin = number
	return v
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
	// AllowEmptyish opts this check into evaluating emptyish ("[]"/null)
	// responses, which the engine otherwise short-circuits. For eth_getLogs an
	// empty result is the everything-dropped corruption shape — exactly what a
	// completeness check must see.
	AllowEmptyish bool
	// Run inspects the decoded response and returns a Violation, nil when the
	// response was verified and satisfies this check, or the Skipped sentinel
	// when the check could not evaluate the response at all (absent data,
	// missing wiring, cold cache) — an absent field is never a violation.
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
	// Feed the config-validation catalog (common can't import this package),
	// so a typo'd check id in config fails validation instead of silently
	// doing nothing.
	common.RegisterIntegrityCheckID(c.ID)
}

// checksFor returns the checks registered for a lowercased method.
func checksFor(method string) []*Check {
	return registry[method]
}
