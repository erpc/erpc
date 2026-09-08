package evm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A chain-identity cordon must not be permanent: once the endpoint answers
// for the configured chain again the poller lifts its own cordon. Until then
// every recheck is a no-op, and a recheck never cordons by itself.
func TestChainIdentityCordon_SelfHealsWhenChainIdMatchesAgain(t *testing.T) {
	up := newSuggestGateUpstream(123, "999", nil)
	p := newGateTestPoller(t, up)
	ctx := context.Background()

	p.recheckChainIdentity(ctx)
	require.False(t, up.isCordoned(), "recheck must never cordon on its own")

	p.SuggestLatestBlock(1000)
	p.SuggestLatestBlock(5_000_000)
	require.Eventually(t, up.isCordoned, 2*time.Second, 10*time.Millisecond)
	require.True(t, p.chainIdCordoned.Load())

	p.recheckChainIdentity(ctx)
	assert.True(t, up.isCordoned(), "still answering for another chain: cordon stays")

	up.setChainId("", assert.AnError)
	p.recheckChainIdentity(ctx)
	assert.True(t, up.isCordoned(), "an unverifiable answer is not a recovery")

	up.setChainId("123", nil)
	p.recheckChainIdentity(ctx)
	assert.False(t, up.isCordoned(), "configured chain observed again: cordon lifted")
	assert.False(t, p.chainIdCordoned.Load())

	p.recheckChainIdentity(ctx)
	assert.False(t, up.isCordoned(), "idempotent once healed")
}
