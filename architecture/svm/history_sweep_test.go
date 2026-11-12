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
// getSignatureStatuses — continue on null when searching history
// ---------------------------------------------------------------------------

func TestPostForward_GetSignatureStatuses_NullContinue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	up := upstreamWithFloor("u", 300000, 200000, true)
	deeper := upstreamWithFloor("archive", 300000, 0, true)

	run := func(t *testing.T, params, result string) (*common.NormalizedResponse, error) {
		t.Helper()
		req := reqOf(t, "getSignatureStatuses", params)
		req.SetUpstreams([]common.Upstream{up, deeper})
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
		require.True(t, common.IsHistorySweepContinuation(err), "must not count as wrong-empty misbehavior")
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
	up := upstreamWithFloor("u", 300000, 200000, true)
	deeper := upstreamWithFloor("archive", 300000, 0, true)

	run := func(t *testing.T, params, result string) (*common.NormalizedResponse, error) {
		t.Helper()
		req := reqOf(t, "getSignaturesForAddress", params)
		req.SetUpstreams([]common.Upstream{up, deeper})
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

// Methods other than the two history reads are untouched.
func TestPostForward_HistorySweep_OtherMethodsUntouched(t *testing.T) {
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
// continue only toward a known deeper floor
// ---------------------------------------------------------------------------

func TestPostForward_HistorySweep_ContinuesOnlyTowardDeeperFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}

	archive := upstreamWithFloor("archive", 300000, 0, true)
	archive2 := upstreamWithFloor("archive2", 300000, 0, true)
	pruned := upstreamWithFloor("pruned", 300000, 200000, true)
	unknown := upstreamWithFloor("unknown", 300000, 0, false)

	sigs := func(u common.Upstream, pool ...common.Upstream) error {
		req := reqOf(t, "getSignaturesForAddress", `["addr",{"limit":5}]`)
		req.SetUpstreams(pool)
		_, err := h.HandleUpstreamPostForward(ctx, net, u, req, resultResp(t, req, sigPage(2)), nil, false)
		return err
	}
	statuses := func(u common.Upstream, pool ...common.Upstream) error {
		req := reqOf(t, "getSignatureStatuses", `[["a"],{"searchTransactionHistory":true}]`)
		req.SetUpstreams(pool)
		_, err := h.HandleUpstreamPostForward(ctx, net, u, req, resultResp(t, req, `{"context":{"slot":9},"value":[null]}`), nil, false)
		return err
	}

	continues := []struct {
		name string
		u    common.Upstream
		pool []common.Upstream
	}{
		{"pruned with a deeper archive selected", pruned, []common.Upstream{pruned, archive}},
		{"pruned with a deeper archive and an unknown", pruned, []common.Upstream{unknown, pruned, archive}},
	}
	for _, tc := range continues {
		t.Run("continues: "+tc.name, func(t *testing.T) {
			t.Parallel()
			for _, err := range []error{sigs(tc.u, tc.pool...), statuses(tc.u, tc.pool...)} {
				require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
				require.True(t, common.IsRetryableTowardNetwork(err))
			}
		})
	}

	serves := []struct {
		name string
		u    common.Upstream
		pool []common.Upstream
	}{
		{"deepest upstream", archive, []common.Upstream{pruned, archive}},
		{"tie at the deepest floor", archive, []common.Upstream{archive, archive2}},
		{"deeper upstream's floor unknown", pruned, []common.Upstream{pruned, unknown}},
		{"own floor unknown", unknown, []common.Upstream{unknown, archive}},
		{"no other upstream selected", pruned, []common.Upstream{pruned}},
		{"no pool on the request", pruned, nil},
	}
	for _, tc := range serves {
		t.Run("serves: "+tc.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, sigs(tc.u, tc.pool...))
			require.NoError(t, statuses(tc.u, tc.pool...))
		})
	}
}

// The history sweep is for serial sweep continuation only. Under consensus, identical
// valid short pages must stay 200s so participants can agree; MissingData would
// collapse to an agreed error with no last-valid-response fallback.
func TestPostForward_HistorySweep_SkippedUnderConsensusSlot(t *testing.T) {
	t.Parallel()
	ctx := common.WithConsensusSlot(context.Background())
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	pruned := upstreamWithFloor("pruned", 300000, 200000, true)
	pool := []common.Upstream{pruned, upstreamWithFloor("archive", 300000, 0, true)}

	t.Run("getSignaturesForAddress short page is served", func(t *testing.T) {
		t.Parallel()
		req := reqOf(t, "getSignaturesForAddress", `["addr",{"limit":5}]`)
		req.SetUpstreams(pool)
		rs, err := h.HandleUpstreamPostForward(ctx, net, pruned, req, resultResp(t, req, sigPage(2)), nil, false)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})

	t.Run("getSignatureStatuses null with history search is served", func(t *testing.T) {
		t.Parallel()
		const withNull = `{"context":{"slot":9},"value":[null]}`
		req := reqOf(t, "getSignatureStatuses", `[["a"],{"searchTransactionHistory":true}]`)
		req.SetUpstreams(pool)
		rs, err := h.HandleUpstreamPostForward(ctx, net, pruned, req, resultResp(t, req, withNull), nil, false)
		require.NoError(t, err)
		require.NotNil(t, rs)
	})
}
