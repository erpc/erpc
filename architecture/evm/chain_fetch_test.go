package evm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// capacityNet answers tracker sweeps as its vendors do: a block whose
// number refuses lists refuses for capacity on every upstream, one in
// fails fails otherwise, and any other answers from vendor-b.
type capacityNet struct {
	common.Network
	mu      sync.Mutex
	refuses map[string]bool
	fails   map[string]bool
}

type capacityNetUpstream struct{ common.Upstream }

func (capacityNetUpstream) Id() string { return "vendor-b" }

func (*capacityNet) ProjectId() string { return "test" }
func (*capacityNet) Label() string     { return "evm:1" }

func (n *capacityNet) set(param string, refuses, fails bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.refuses[param], n.fails[param] = refuses, fails
}

func (n *capacityNet) SweepUpstreams(ctx context.Context, req *common.NormalizedRequest, _ string, check func(*common.NormalizedResponse) error) (*common.NormalizedResponse, error) {
	body := string(req.Body())
	n.mu.Lock()
	defer n.mu.Unlock()
	for param, refuses := range n.refuses {
		if !strings.Contains(body, `"`+param+`"`) {
			continue
		}
		if refuses {
			return nil, common.NewErrUpstreamsExhaustedWithCause(errors.Join(
				common.NewErrEndpointCapacityExceeded(errors.New("vendor-a: 429")),
				common.NewErrEndpointCapacityExceeded(errors.New("vendor-b: -32005 compute units")),
			))
		}
		if n.fails[param] {
			return nil, common.NewErrUpstreamsExhaustedWithCause(errors.Join(
				common.NewErrEndpointServerSideException(errors.New("vendor-b: 500"), nil, 500),
			))
		}
	}
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xok"`), nil)).SetUpstream(capacityNetUpstream{})
	return resp, check(resp)
}

// A sweep refused for capacity everywhere halves the lowest busy class
// with one fetch in flight; a vendor's answer ends every capacity backoff
// at once and keeps other failures' backoff (spec §8, §19 U5, U6).
func TestFetchScheduler_CapacityRefusals(t *testing.T) {
	min0 := fetchBackoffMin
	fetchBackoffMin = time.Minute
	t.Cleanup(func() { fetchBackoffMin = min0 })

	net := &capacityNet{refuses: map[string]bool{}, fails: map[string]bool{}}
	s := newFetchScheduler(t.Context(), net)
	spec := func(class fetchClass, param string) fetchSpec {
		return fetchSpec{class: class, kind: "header", method: "eth_getBlockByNumber", params: `["` + param + `",false]`,
			decode: func(b []byte) (any, error) { return string(b), nil }}
	}
	share := func(c fetchClass) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.share[c]
	}
	fetch := func(class fetchClass, param string, within time.Duration) (fetchResult, bool) {
		ctx, cancel := context.WithTimeout(t.Context(), within)
		defer cancel()
		res := s.fetch(ctx, nil, spec(class, param))
		return res, ctx.Err() == nil
	}

	net.set("0x1", true, false)
	net.set("0x3", false, true)
	res, done := fetch(classFill, "0x1", time.Second)
	require.True(t, done)
	require.Equal(t, "capacity", fetchOutcome(res.err))
	require.Equal(t, fetchSlots/2, share(classFill), "one refused sweep in flight halves its class")
	require.Equal(t, fetchSlots, share(classTipReceipts), "a busier class keeps its share")

	res, done = fetch(classHead, "0x3", time.Second)
	require.True(t, done)
	require.Equal(t, "error", fetchOutcome(res.err))

	// Both keys back off for a minute; capacity returns.
	net.set("0x1", false, false)
	net.set("0x3", false, false)
	refused := s.submit(nil, spec(classFill, "0x1"))
	failed := s.submit(nil, spec(classHead, "0x3"))
	select {
	case <-refused.r.done:
		t.Fatal("a capacity backoff ended before any vendor answered")
	case <-time.After(100 * time.Millisecond):
	}

	res, done = fetch(classTipReceipts, "0x2", time.Second)
	require.True(t, done)
	require.NoError(t, res.err)
	select {
	case <-refused.r.done:
		require.NoError(t, refused.r.res.err)
	case <-time.After(time.Second):
		t.Fatal("a vendor answered, yet the capacity backoff still holds its key")
	}
	select {
	case <-failed.r.done:
		t.Fatal("a vendor's answer ended a backoff that was not for capacity")
	case <-time.After(200 * time.Millisecond):
	}
	failed.leave()
}
