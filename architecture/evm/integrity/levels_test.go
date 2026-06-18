package integrity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLevelMembershipCoversAllChecks is the drift guard: every registered check
// must be assigned to exactly one level, and the level table may not reference
// an unknown check id. Adding a check without placing it in a level (or a typo
// in the table) fails here.
func TestLevelMembershipCoversAllChecks(t *testing.T) {
	inLevel := map[string]bool{}
	for _, ids := range levelMembership {
		for _, id := range ids {
			assert.False(t, inLevel[id], "check %q listed in more than one level", id)
			inLevel[id] = true
		}
	}

	registered := map[string]bool{}
	for _, c := range allChecks {
		registered[c.ID] = true
		assert.True(t, inLevel[c.ID], "registered check %q is not assigned to any level", c.ID)
	}
	for id := range inLevel {
		assert.True(t, registered[id], "level table references unknown check %q", id)
	}
}

func TestCheckSetForLevel(t *testing.T) {
	assert.Empty(t, CheckSetForLevel(LevelOff), "off enables nothing")
	assert.Empty(t, CheckSetForLevel(Level("nonsense")), "unknown level enables nothing")

	intrinsic := CheckSetForLevel(LevelIntrinsic)
	assert.True(t, intrinsic.For("indexMagnitude").Enabled)
	assert.False(t, intrinsic.For("expectedBlock").Enabled, "corroborated check absent from intrinsic")
	assert.False(t, intrinsic.For("receiptVsBlock").Enabled, "authoritative check absent from intrinsic")

	corroborated := CheckSetForLevel(LevelCorroborated)
	assert.True(t, corroborated.For("indexMagnitude").Enabled, "includes all lower levels")
	assert.True(t, corroborated.For("expectedBlock").Enabled)
	assert.False(t, corroborated.For("receiptVsBlock").Enabled, "authoritative still excluded")

	authoritative := CheckSetForLevel(LevelAuthoritative)
	assert.True(t, authoritative.For("indexMagnitude").Enabled)
	assert.True(t, authoritative.For("expectedBlock").Enabled)
	assert.True(t, authoritative.For("receiptVsBlock").Enabled, "the force-fetch tier is on")
}
