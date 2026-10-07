package erpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// scriptedHeadUpstream answers eth_blockNumber with head, or fails.
type scriptedHeadUpstream struct {
	*common.FakeUpstream
	head  int64
	fail  bool
	calls int
}

func (u *scriptedHeadUpstream) Forward(context.Context, *common.NormalizedRequest, bool, bool) (*common.NormalizedResponse, error) {
	u.calls++
	if u.fail {
		return nil, errors.New("429 too many requests")
	}
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte("1"), []byte(fmt.Sprintf(`"0x%x"`, u.head)), nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
}

// A failed tip check (timeout, 429) must not mark a healthy upstream behind
// for the whole tracker-head epoch: it is retried after a short delay, while
// a successful answer is still reused for the epoch.
func TestHeadTracker_FailedTipCheckIsNotCachedForTheEpoch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ht := newTestTracker(newTestSSR(t, t.Context()), nil, headTrackerDeps{now: func() time.Time { return now }})
	base := common.NewFakeUpstream("u1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(90, 0))).(*common.FakeUpstream)
	u := &scriptedHeadUpstream{FakeUpstream: base, head: 100, fail: true}
	const tracked = int64(100)

	require.Equal(t, int64(90), ht.checkUpstreamHead(t.Context(), u, tracked, "p", "n"), "a failed check falls back to the known head")
	require.Equal(t, 1, u.calls)
	require.Equal(t, int64(90), ht.checkUpstreamHead(t.Context(), u, tracked, "p", "n"), "failures are rate limited briefly")
	require.Equal(t, 1, u.calls)

	u.fail = false
	now = now.Add(headTrackerTipCheckRetry)
	require.Equal(t, int64(100), ht.checkUpstreamHead(t.Context(), u, tracked, "p", "n"), "the same epoch is re-checked after the retry delay")
	require.Equal(t, 2, u.calls)

	now = now.Add(time.Hour)
	require.Equal(t, int64(100), ht.checkUpstreamHead(t.Context(), u, tracked, "p", "n"), "a successful check holds for the epoch")
	require.Equal(t, 2, u.calls)
	u.head = 101
	require.Equal(t, int64(101), ht.checkUpstreamHead(t.Context(), u, tracked+1, "p", "n"), "a new tracked head re-checks")
	require.Equal(t, 3, u.calls)
}
