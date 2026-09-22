package common

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A MissingData verdict with no permanent flag is transient by default: it may
// be a not-yet-indexed block that appears as the chain tip advances, so a
// wait-and-retry can still help. Pins the `default false` contract of
// IsPermanentlyMissingData (errors.go).
func TestIsPermanentlyMissingData_PlainMissingDataIsTransient(t *testing.T) {
	md := NewErrEndpointMissingData(errors.New("x"), nil)
	assert.False(t, IsPermanentlyMissingData(md),
		"an unflagged MissingData error defaults to transient")
}

// WithPermanentMissingData(true) promotes onto *ErrEndpointMissingData and marks
// the verdict permanent (a skipped/absent slot that no re-fetch can resurrect).
func TestIsPermanentlyMissingData_FlaggedIsPermanent(t *testing.T) {
	md := NewErrEndpointMissingData(errors.New("x"), nil)
	md.(*ErrEndpointMissingData).WithPermanentMissingData(true)
	assert.True(t, IsPermanentlyMissingData(md),
		"WithPermanentMissingData(true) must make the verdict permanent")
}

func TestIsPermanentlyMissingData_NilAndNonStandard(t *testing.T) {
	assert.False(t, IsPermanentlyMissingData(nil),
		"nil is never permanently-missing")
	assert.False(t, IsPermanentlyMissingData(errors.New("x")),
		"a non-StandardError can never carry the permanent flag")
}

// Do-not-descend invariant: the permanent flag lives on each individual
// MissingData cause, never on the ErrUpstreamsExhausted / errors.Join wrapper.
// IsPermanentlyMissingData walks only the single-cause chain (mirroring
// IsRetryableTowardNetwork) and stops before entering a multi-error fan-out, so
// the wrapper itself reports false EVEN when every cause is permanent. This is
// deliberate: the network layer (shouldRetryWithReason) scans ue.Errors()
// cause-by-cause instead — a fan-out descent here would resurrect the
// order-dependent bug DeepSearch had. The test would flip to true (and catch
// the regression) if the walk were ever made to descend into the wrapper.
func TestIsPermanentlyMissingData_DoesNotDescendIntoMultiError(t *testing.T) {
	permMD1 := NewErrEndpointMissingData(errors.New("-32007 skipped 1"), nil)
	permMD1.(*ErrEndpointMissingData).WithPermanentMissingData(true)
	permMD2 := NewErrEndpointMissingData(errors.New("-32007 skipped 2"), nil)
	permMD2.(*ErrEndpointMissingData).WithPermanentMissingData(true)

	exhausted := NewErrUpstreamsExhaustedWithCause(errors.Join(permMD1, permMD2))

	assert.False(t, IsPermanentlyMissingData(exhausted),
		"must not descend into a multi-error wrapper even when every cause is permanent")
	// Sanity: the flag itself works on the individual causes.
	assert.True(t, IsPermanentlyMissingData(permMD1))
	assert.True(t, IsPermanentlyMissingData(permMD2))
}
