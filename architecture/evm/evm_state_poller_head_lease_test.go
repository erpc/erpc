package evm

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

type fakeLease struct{ held atomic.Bool }

func (l *fakeLease) Held() bool { return l.held.Load() }

// headUpstream serves a fixed (settable) latest/finalized height and counts calls.
type headUpstream struct {
	*suggestGateUpstream
	height atomic.Int64
	calls  atomic.Int64
}

func (u *headUpstream) Forward(ctx context.Context, _ *common.NormalizedRequest, _ bool, _ bool) (*common.NormalizedResponse, error) {
	u.calls.Add(1)
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{
		"number": fmt.Sprintf("0x%x", u.height.Load()), "timestamp": "0x1",
	}, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
}

func newLeasePoller(t *testing.T, h int64) (*EvmStatePoller, *headUpstream) {
	up := &headUpstream{suggestGateUpstream: newSuggestGateUpstream(123, "0x7b", nil)}
	up.height.Store(h)
	p := newGateTestPoller(t, up)
	p.debounceInterval = time.Millisecond
	return p, up
}

func TestEvmStatePoller_HeadPollLease_FollowerSkipsWhileFresh(t *testing.T) {
	ctx := context.Background()
	p, up := newLeasePoller(t, 100)
	lease := &fakeLease{}
	var skips atomic.Int64
	p.SetHeadPollLease(lease, 200*time.Millisecond, func() { skips.Add(1) })

	// Unobserved upstream: stale shared state, follower fails open and polls.
	v, err := p.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(100), v)
	_, err = p.PollFinalizedBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), up.calls.Load())
	require.Zero(t, skips.Load())

	// Fresh shared state and another holder: both latest and finalized skip,
	// return the shared value, and do not refresh the timestamp.
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		v, err = p.PollLatestBlockNumber(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(100), v)
		_, err = p.PollFinalizedBlockNumber(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, int64(2), up.calls.Load(), "no upstream calls while skipping")
	require.Equal(t, int64(6), skips.Load())

	// Skips never refresh freshness: after staleAfter the follower polls again.
	time.Sleep(250 * time.Millisecond)
	require.True(t, p.latestBlockShared.IsStale(200*time.Millisecond))
	_, err = p.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), up.calls.Load())
}

func TestEvmStatePoller_HeadPollLease_HolderStalledChainStaysFresh(t *testing.T) {
	ctx := context.Background()
	p, up := newLeasePoller(t, 500)
	lease := &fakeLease{}
	lease.held.Store(true)
	p.SetHeadPollLease(lease, 150*time.Millisecond, nil)

	// Holder polls every cycle even though the chain is stuck at one height;
	// each genuine observation keeps the shared counter fresh.
	for i := 0; i < 5; i++ {
		v, err := p.PollLatestBlockNumber(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(500), v)
		time.Sleep(60 * time.Millisecond)
		require.False(t, p.latestBlockShared.IsStale(150*time.Millisecond), "same-height observation must keep freshness")
	}
	require.Equal(t, int64(5), up.calls.Load(), "holder polls every cycle")
}

func TestEvmStatePoller_HeadPollLease_NilLeaseUnchanged(t *testing.T) {
	ctx := context.Background()
	p, up := newLeasePoller(t, 7)
	p.SetHeadPollLease(nil, time.Hour, func() { t.Fatal("must not skip without lease") })
	for i := 0; i < 3; i++ {
		_, err := p.PollLatestBlockNumber(ctx)
		require.NoError(t, err)
		time.Sleep(3 * time.Millisecond)
	}
	require.Equal(t, int64(3), up.calls.Load())
}
