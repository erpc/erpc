package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The selection-reason context markers must survive context derivation
// (WithCancel/WithValue wrapping between the tagging executor and the
// attempt recorder) and must read false on untagged contexts.
func TestSelectionReasonContextMarkers(t *testing.T) {
	bare := context.Background()
	assert.False(t, IsConsensusSlot(bare))
	assert.False(t, IsSweepIteration(bare))

	slot := WithConsensusSlot(bare)
	assert.True(t, IsConsensusSlot(slot))
	assert.False(t, IsSweepIteration(slot), "markers are independent")

	sweep := WithSweepIteration(bare)
	assert.True(t, IsSweepIteration(sweep))
	assert.False(t, IsConsensusSlot(sweep), "markers are independent")

	// Derivation between tag and read is the normal case: the consensus
	// executor wraps the slot ctx in WithCancel before handing it down.
	derived, cancel := context.WithCancel(slot)
	defer cancel()
	assert.True(t, IsConsensusSlot(derived))

	// The marker never leaks upward to the parent.
	assert.False(t, IsConsensusSlot(bare))
}
