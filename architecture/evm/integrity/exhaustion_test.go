package integrity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The detector must fire on the all-upstream signature (many exhaustions in a
// short window) and stay quiet for the isolated, expected ones — a check that
// rejects and still gets a good response elsewhere never reaches here at all.

func TestRecordExhaustion_FiresOnlyOnTheSignature(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	exhaustionClock = func() time.Time { return now }

	for i := 1; i < exhaustionThreshold; i++ {
		report, count := RecordExhaustion("hyperevm", "transactionsRootConsistency")
		assert.False(t, report, "must not report below the threshold (event %d)", i)
		assert.Equal(t, i, count)
	}
	report, count := RecordExhaustion("hyperevm", "transactionsRootConsistency")
	assert.True(t, report, "the threshold event completes the all-upstream signature")
	assert.Equal(t, exhaustionThreshold, count)
}

func TestRecordExhaustion_StaleEventsAgeOut(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	exhaustionClock = func() time.Time { return now }

	// A near-threshold burst...
	for i := 0; i < exhaustionThreshold-1; i++ {
		RecordExhaustion("polygon", "baseFeeDerivation")
	}
	// ...then a long quiet period: the burst must not combine with a later one.
	now = now.Add(exhaustionWindow + time.Minute)
	report, count := RecordExhaustion("polygon", "baseFeeDerivation")
	assert.False(t, report, "events older than the window must not accumulate")
	assert.Equal(t, 1, count, "only the fresh event remains in the window")
}

func TestRecordExhaustion_IsolatedFailuresStayQuiet(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	exhaustionClock = func() time.Time { return now }

	// One exhaustion every window+ — the "rare genuine catch that happened to
	// exhaust" pattern. Must never report.
	for i := 0; i < 20; i++ {
		report, _ := RecordExhaustion("mainnet", "parentHashLinkage")
		require.False(t, report, "isolated exhaustions are not the signature")
		now = now.Add(exhaustionWindow + time.Second)
	}
}

func TestRecordExhaustion_KeyedPerNetworkAndCheck(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	exhaustionClock = func() time.Time { return now }

	// Spreading the same number of events across distinct keys must not trip
	// any of them — a real protocol-invalid check concentrates on one key.
	for i := 0; i < exhaustionThreshold; i++ {
		r1, _ := RecordExhaustion("bsc", "baseFeeDerivation")
		r2, _ := RecordExhaustion("bsc", "hashStability")
		r3, _ := RecordExhaustion("polygon", "baseFeeDerivation")
		if i < exhaustionThreshold-1 {
			assert.False(t, r1)
			assert.False(t, r2)
			assert.False(t, r3)
		}
	}
	// Each key independently reaches the threshold on its own final event.
	resetExhaustionForTest()
	exhaustionClock = func() time.Time { return now }
	for i := 1; i <= exhaustionThreshold; i++ {
		r, c := RecordExhaustion("bsc", "baseFeeDerivation")
		assert.Equal(t, i, c)
		assert.Equal(t, i == exhaustionThreshold, r)
	}
	r, c := RecordExhaustion("bsc", "hashStability")
	assert.False(t, r, "a different check on the same network is tracked separately")
	assert.Equal(t, 1, c)
}

func TestRecordExhaustion_RealertThrottled(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	exhaustionClock = func() time.Time { return now }

	fire := func() bool {
		var reported bool
		for i := 0; i < exhaustionThreshold; i++ {
			r, _ := RecordExhaustion("hyperevm", "transactionsRootConsistency")
			reported = reported || r
		}
		return reported
	}
	assert.True(t, fire(), "first crossing reports")
	assert.False(t, fire(), "a persistently broken check must not report on every request")

	now = now.Add(exhaustionRealertAfter + time.Minute)
	assert.True(t, fire(), "reports again after the re-alert cooldown")
}

func TestRecordExhaustion_IgnoresEmptyKeys(t *testing.T) {
	resetExhaustionForTest()
	defer resetExhaustionForTest()
	r, c := RecordExhaustion("", "someCheck")
	assert.False(t, r)
	assert.Zero(t, c)
	r, c = RecordExhaustion("mainnet", "")
	assert.False(t, r)
	assert.Zero(t, c)
}
