// Package policy holds the unified upstream-selection engine. The engine
// runs a per-(network, method) eval on a tick, snapshots the result into an
// atomic cache, and serves the request-path through a wait-free read.
// See specs/selection-policy/feature.md for the full design.
package policy

import "errors"

// ErrNoEligibleUpstream is returned by Engine.GetOrdered when the slot's
// cache holds an empty list — typically because the user's eval returned
// no upstreams, or because no eval has succeeded yet.
var ErrNoEligibleUpstream = errors.New("no eligible upstream returned by selection policy")

// ErrEvalTimeout is logged (but not returned to callers) when an eval
// exceeds `selectionPolicy.evalTimeout`. The prior cache is retained.
var ErrEvalTimeout = errors.New("selection policy eval timed out")

// ErrInvalidReturn is logged when the eval returns a non-array or an entry
// that does not correspond to a known upstream id.
var ErrInvalidReturn = errors.New("selection policy eval returned an invalid value")

// ErrSlotUnknown is returned by accessors when a (network, method) slot is
// not registered.
var ErrSlotUnknown = errors.New("selection policy slot not registered")
