package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Upstream.Forward can end a request with no response and no error. It logs
// that pair as "upstream request ended with nil response and nil error" and
// returns it. NormalizedResponse.JsonRpcResponse answers (nil, nil) for a nil
// receiver, so the poll helpers read a nil *JsonRpcResponse. The
// suggestGateUpstream double answers every Forward with (nil, nil).

// fetchBlock must report "no block", the same answer an empty result gives.
// Its callers already count that as a failed poll.
func TestFetchBlock_NilResponseReportsNoBlock(t *testing.T) {
	p := newGateTestPoller(t, newSuggestGateUpstream(123, "123", nil))

	b, err := p.fetchBlock(context.Background(), "latest", common.UpstreamPoliciesHeadAsk)

	require.NoError(t, err)
	assert.Equal(t, int64(0), b.number)
	assert.Equal(t, int64(0), b.timestamp)
}

// fetchSyncingState has no neutral answer: false claims the node is synced.
// A nil response tells the poller nothing, so it must be an error.
func TestFetchSyncingState_NilResponseIsAnError(t *testing.T) {
	p := newGateTestPoller(t, newSuggestGateUpstream(123, "123", nil))

	syncing, err := p.fetchSyncingState(context.Background())

	require.Error(t, err)
	assert.False(t, syncing)
	assert.True(t, common.HasErrorCode(err, "ErrEvmStatePoller"))
}
