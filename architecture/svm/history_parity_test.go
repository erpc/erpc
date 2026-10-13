package svm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func resultResp(t *testing.T, req *common.NormalizedRequest, resultJSON string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(resultJSON), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func sigPage(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"signature":"sig%d","slot":%d,"err":null}`, i, 100+i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// ---------------------------------------------------------------------------
// getFirstAvailableBlock — pool-answered
// ---------------------------------------------------------------------------

func TestNetworkPreForward_GetFirstAvailableBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	req := func() *common.NormalizedRequest { return reqOf(t, "getFirstAvailableBlock", `[]`) }

	t.Run("mixed pruned validator and archive answers the MIN", func(t *testing.T) {
		t.Parallel()
		pool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, true),
		}
		handled, resp, err := networkPreForward_getFirstAvailableBlock(ctx, pool, req())
		require.NoError(t, err)
		require.True(t, handled)
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		require.Equal(t, "0", string(jrr.GetResultBytes()), "archive floor 0 is KNOWN and is the pool MIN")
	})

	t.Run("all pruned answers the lowest pruned floor", func(t *testing.T) {
		t.Parallel()
		pool := []common.Upstream{
			upstreamWithFloor("v1", 300000, 250000, true),
			upstreamWithFloor("v2", 300000, 200000, true),
		}
		handled, resp, err := networkPreForward_getFirstAvailableBlock(ctx, pool, req())
		require.NoError(t, err)
		require.True(t, handled)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		require.Equal(t, "200000", string(jrr.GetResultBytes()))
	})

	t.Run("one unknown floor fails open", func(t *testing.T) {
		t.Parallel()
		pool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, false),
		}
		handled, resp, err := networkPreForward_getFirstAvailableBlock(ctx, pool, req())
		require.NoError(t, err)
		require.False(t, handled, "a partial MIN must not be advertised as pool truth")
		require.Nil(t, resp)
	})

	t.Run("uninspectable upstream fails open", func(t *testing.T) {
		t.Parallel()
		pool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			&slotLagUpstream{id: "no-poller", poller: nil},
		}
		handled, _, err := networkPreForward_getFirstAvailableBlock(ctx, pool, req())
		require.NoError(t, err)
		require.False(t, handled)
	})

	t.Run("single upstream forwards as today", func(t *testing.T) {
		t.Parallel()
		handled, _, err := networkPreForward_getFirstAvailableBlock(ctx,
			[]common.Upstream{upstreamWithFloor("only", 300000, 200000, true)}, req())
		require.NoError(t, err)
		require.False(t, handled)
	})

	t.Run("empty pool forwards as today", func(t *testing.T) {
		t.Parallel()
		handled, _, err := networkPreForward_getFirstAvailableBlock(ctx, nil, req())
		require.NoError(t, err)
		require.False(t, handled)
	})

	t.Run("internal requests are never answered locally", func(t *testing.T) {
		t.Parallel()
		pool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 0, true),
		}
		r := req()
		r.SetDirectives(&common.RequestDirectives{IsInternal: true})
		handled, _, err := networkPreForward_getFirstAvailableBlock(ctx, pool, r)
		require.NoError(t, err)
		require.False(t, handled)
	})

	t.Run("dispatched through the handler", func(t *testing.T) {
		t.Parallel()
		h := &SvmArchitectureHandler{}
		pool := []common.Upstream{
			upstreamWithFloor("validator", 300000, 200000, true),
			upstreamWithFloor("archive", 300000, 5, true),
		}
		handled, resp, err := h.HandleNetworkPreForward(ctx, &fakeNetwork{}, pool, req())
		require.NoError(t, err)
		require.True(t, handled)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		require.Equal(t, "5", string(jrr.GetResultBytes()))
	})
}

func TestNeverCache_GetFirstAvailableBlock(t *testing.T) {
	t.Parallel()
	require.True(t, neverCacheMethods["getFirstAvailableBlock"])
}

// ---------------------------------------------------------------------------
// getSignatureStatuses — continue on null when searching history
// ---------------------------------------------------------------------------

func TestPostForward_GetSignatureStatuses_NullContinue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	up := upstreamWithFloor("u", 300000, 0, true)

	run := func(t *testing.T, params, result string) (*common.NormalizedResponse, error) {
		t.Helper()
		req := reqOf(t, "getSignatureStatuses", params)
		return h.HandleUpstreamPostForward(ctx, net, up, req, resultResp(t, req, result), nil, false)
	}

	const withNull = `{"context":{"slot":9},"value":[{"slot":1,"confirmations":null,"err":null,"confirmationStatus":"finalized"},null]}`
	const allFound = `{"context":{"slot":9},"value":[{"slot":1,"confirmations":null,"err":null,"confirmationStatus":"finalized"}]}`

	t.Run("flag true + null element is MissingData", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `[["a","b"],{"searchTransactionHistory":true}]`, withNull)
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
		require.True(t, common.IsRetryableTowardNetwork(err), "the sweep must continue to the next upstream")
		require.True(t, common.IsPermanentlyMissingData(err), "waiting cannot make a signature land: no delayed re-sweep")
	})

	t.Run("flag true + all non-null is served", func(t *testing.T) {
		t.Parallel()
		rs, err := run(t, `[["a"],{"searchTransactionHistory":true}]`, allFound)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("flag true + empty value is served", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `[[],{"searchTransactionHistory":true}]`, `{"context":{"slot":9},"value":[]}`)
		require.NoError(t, err)
	})

	t.Run("flag false + null is a valid recent-status answer", func(t *testing.T) {
		t.Parallel()
		rs, err := run(t, `[["a","b"],{"searchTransactionHistory":false}]`, withNull)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("flag absent + null is a valid recent-status answer", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `[["a","b"]]`, withNull)
		require.NoError(t, err)
		_, err = run(t, `[["a","b"],{}]`, withNull)
		require.NoError(t, err)
	})

	t.Run("non-boolean flag is not treated as true", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `[["a","b"],{"searchTransactionHistory":"true"}]`, withNull)
		require.NoError(t, err)
	})

	t.Run("upstream error passes through untouched", func(t *testing.T) {
		t.Parallel()
		req := reqOf(t, "getSignatureStatuses", `[["a"],{"searchTransactionHistory":true}]`)
		upErr := common.NewErrEndpointServerSideException(nil, nil, 500)
		_, err := h.HandleUpstreamPostForward(ctx, net, up, req, nil, upErr, false)
		require.Same(t, upErr, err)
	})

	t.Run("malformed result fails open", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `[["a"],{"searchTransactionHistory":true}]`, `"not-an-envelope"`)
		require.NoError(t, err)
	})
}

// ---------------------------------------------------------------------------
// getSignaturesForAddress — short page is MissingData
// ---------------------------------------------------------------------------

func TestPostForward_GetSignaturesForAddress_ShortPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	up := upstreamWithFloor("u", 300000, 0, true)

	run := func(t *testing.T, params, result string) (*common.NormalizedResponse, error) {
		t.Helper()
		req := reqOf(t, "getSignaturesForAddress", params)
		return h.HandleUpstreamPostForward(ctx, net, up, req, resultResp(t, req, result), nil, false)
	}

	t.Run("limit N with N-1 results is MissingData", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `["addr",{"limit":5}]`, sigPage(4))
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
		require.True(t, common.IsRetryableTowardNetwork(err))
		require.True(t, common.IsPermanentlyMissingData(err), "waiting cannot lengthen history: no delayed re-sweep")
	})

	t.Run("limit N with N results is accepted", func(t *testing.T) {
		t.Parallel()
		rs, err := run(t, `["addr",{"limit":5}]`, sigPage(5))
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("limit with commitment injected is still honoured", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `["addr",{"limit":3,"commitment":"finalized"}]`, sigPage(2))
		require.Error(t, err)
		_, err = run(t, `["addr",{"limit":3,"commitment":"finalized"}]`, sigPage(3))
		require.NoError(t, err)
	})

	t.Run("omitted limit uses the 1000 default", func(t *testing.T) {
		t.Parallel()
		for _, params := range []string{`["addr"]`, `["addr",{"commitment":"finalized"}]`, `["addr",{"before":"x"}]`} {
			_, err := run(t, params, sigPage(999))
			require.Errorf(t, err, "999 < 1000 must be short for %s", params)
			require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))

			rs, err := run(t, params, sigPage(1000))
			require.NoErrorf(t, err, "a full 1000-page must be accepted for %s", params)
			require.NotNil(t, rs)
		}
	})

	t.Run("empty page with limit > 0 continues", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `["addr",{"limit":10}]`, `[]`)
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})

	t.Run("non-array result fails open", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, `["addr",{"limit":10}]`, `null`)
		require.NoError(t, err)
		_, err = run(t, `["addr",{"limit":10}]`, `{"a":1}`)
		require.NoError(t, err)
	})

	t.Run("invalid limit is not interpreted", func(t *testing.T) {
		t.Parallel()
		for _, limit := range []string{`0`, `-1`, `1.5`, `"5"`, `null`} {
			_, err := run(t, fmt.Sprintf(`["addr",{"limit":%s}]`, limit), sigPage(2))
			require.NoErrorf(t, err, "limit %s must fail open", limit)
		}
	})

	// The error classes that already fail over keep doing so: the hook only
	// inspects successful responses and never rewrites an upstream error.
	t.Run("upstream errors pass through untouched", func(t *testing.T) {
		t.Parallel()
		req := reqOf(t, "getSignaturesForAddress", `["addr",{"limit":5}]`)
		for _, code := range []int{-32020, -32011} {
			upErr := extract(t, code, "history unavailable", 200)
			gotRs, err := h.HandleUpstreamPostForward(ctx, net, up, req, nil, upErr, false)
			require.Nil(t, gotRs)
			require.Same(t, upErr, err)
			require.Truef(t, common.IsRetryableTowardNetwork(err), "%d must still fail over", code)
		}
		require.True(t, common.HasErrorCode(extract(t, -32011, "x", 200), common.ErrCodeEndpointMissingData))
	})
}

// Methods outside the two history-parity entries are untouched.
func TestPostForward_HistoryParity_OtherMethodsUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	up := upstreamWithFloor("u", 300000, 0, true)
	req := reqOf(t, "getTransaction", `["sig"]`)
	_, err := h.HandleUpstreamPostForward(ctx, net, up, req, resultResp(t, req, `null`), nil, false)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// deepest-history exemption
// ---------------------------------------------------------------------------

func TestPostForward_HistoryParity_DeepestHistoryIsAuthoritative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}

	archive := upstreamWithFloor("archive", 300000, 0, true)
	archive2 := upstreamWithFloor("archive2", 300000, 0, true)
	pruned := upstreamWithFloor("pruned", 300000, 200000, true)
	unknown := upstreamWithFloor("unknown", 300000, 0, false)

	sigs := func(u common.Upstream, pool ...common.Upstream) (*common.NormalizedResponse, error) {
		req := reqOf(t, "getSignaturesForAddress", `["addr",{"limit":5}]`)
		req.SetUpstreams(pool)
		return h.HandleUpstreamPostForward(ctx, net, u, req, resultResp(t, req, sigPage(2)), nil, false)
	}
	const withNull = `{"context":{"slot":9},"value":[null]}`
	statuses := func(u common.Upstream, pool ...common.Upstream) (*common.NormalizedResponse, error) {
		req := reqOf(t, "getSignatureStatuses", `[["a"],{"searchTransactionHistory":true}]`)
		req.SetUpstreams(pool)
		return h.HandleUpstreamPostForward(ctx, net, u, req, resultResp(t, req, withNull), nil, false)
	}

	t.Run("pool-deepest upstream's short page is served", func(t *testing.T) {
		t.Parallel()
		rs, err := sigs(archive, pruned, archive)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("pool-deepest upstream's null status is served", func(t *testing.T) {
		t.Parallel()
		rs, err := statuses(archive, pruned, archive)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("a tie at the pool floor is authoritative", func(t *testing.T) {
		t.Parallel()
		_, err := sigs(archive, archive, archive2)
		require.NoError(t, err)
		_, err = statuses(archive2, archive, archive2)
		require.NoError(t, err)
	})

	t.Run("a shallower upstream still sweeps on", func(t *testing.T) {
		t.Parallel()
		_, err := sigs(pruned, pruned, archive)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
		require.True(t, common.IsRetryableTowardNetwork(err))
		_, err = statuses(pruned, pruned, archive)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})

	t.Run("an unknown pool member's floor keeps the sweep going", func(t *testing.T) {
		t.Parallel()
		_, err := sigs(archive, archive, unknown)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
		_, err = statuses(archive, archive, unknown)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})

	t.Run("the upstream's own unknown floor keeps the sweep going", func(t *testing.T) {
		t.Parallel()
		_, err := sigs(unknown, unknown, archive)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})

	t.Run("no pool on the request fails open to the sweep", func(t *testing.T) {
		t.Parallel()
		_, err := sigs(archive)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})
}
