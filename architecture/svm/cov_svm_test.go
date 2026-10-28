package svm

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// covSvmBadBody is a request body that cannot be parsed into a JSON-RPC
// envelope, so req.Method() and req.JsonRpcRequest() both fail.
const covSvmBadBody = `{not json`

func covSvmBadReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(covSvmBadBody))
}

func covSvmReqFromParams(method string, params []interface{}) *common.NormalizedRequest {
	return common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(method, params))
}

func covSvmResultResp(t *testing.T, req *common.NormalizedRequest, raw string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(raw), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func covSvmErrorResp(t *testing.T, req *common.NormalizedRequest, code int, msg string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, ""))
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

// ---- handler / unparseable requests -------------------------------------------

// Every entry point that resolves the method must surface (or tolerate) an
// unparseable body without fabricating a result.
func TestCovSvm_UnparseableRequest_EveryEntryPoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta", Commitment: "finalized"},
	}}

	handled, resp, err := h.HandleProjectPreForward(ctx, net, covSvmBadReq())
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	require.False(t, handled)
	require.Nil(t, resp)

	handled, resp, err = h.HandleNetworkPreForward(ctx, net, nil, covSvmBadReq())
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	require.False(t, handled)
	require.Nil(t, resp)

	// Post-forward hooks never invent an error: the upstream outcome passes through.
	goodReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`))
	upResp := covSvmResultResp(t, goodReq, `5`)
	got, err := h.HandleNetworkPostForward(ctx, net, covSvmBadReq(), upResp, nil)
	require.NoError(t, err)
	require.Same(t, upResp, got)

	upErr := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 502)
	got, err = h.HandleUpstreamPostForward(ctx, net, &svmUpstreamStub{}, covSvmBadReq(), upResp, upErr, false)
	require.Same(t, upErr, err)
	require.Same(t, upResp, got)

	// Commitment helpers degrade to their "unknown" values.
	require.Equal(t, "", effectiveCommitment(ctx, net, covSvmBadReq()))
	require.Equal(t, "", effectiveCommitment(ctx, net, nil))
	require.Equal(t, "none", FailsafeCommitment(ctx, net, covSvmBadReq()))
	c, action, idx := resolveCommitment(ctx, net, covSvmBadReq())
	require.Equal(t, "", c)
	require.Equal(t, commitmentSkip, action)
	require.Equal(t, -1, idx)

	handled, _, err = networkPreForward_injectWriteCommitment(ctx, net, covSvmBadReq())
	require.NoError(t, err)
	require.False(t, handled)

	// Slot helpers fail open / report "no bound".
	end, ok := RequestRangeEndSlot(ctx, covSvmBadReq(), 7, 100)
	require.True(t, ok)
	require.EqualValues(t, 7, end, "unknown method resolves to start itself")
	_, ok = RequestLowerBoundSlot(ctx, covSvmBadReq())
	require.False(t, ok)
	require.Zero(t, MinContextSlotOf(ctx, covSvmBadReq()))
	require.Zero(t, MinContextSlotOf(ctx, nil))

	// Finality cannot be resolved without a method.
	require.Equal(t, common.DataFinalityStateUnknown, GetFinality(ctx, net, covSvmBadReq(), nil))
	require.Equal(t, common.DataFinalityStateUnknown, GetFinality(ctx, net, nil, nil))

	// Request identity cannot be derived either.
	_, err = RequestKey(ctx, covSvmBadReq())
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	_, err = RequestKey(ctx, nil)
	require.ErrorContains(t, err, "nil request")
	_, err = svmRequestKey(nil)
	require.ErrorContains(t, err, "nil json-rpc request")
}

// A context-slot response on a bad request must not feed the poller.
func TestCovSvm_TrackContextSlot_Guards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	newBalanceReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["Abc"]}`))
	}

	cases := []struct {
		name string
		req  func() *common.NormalizedRequest
		resp func(req *common.NormalizedRequest) *common.NormalizedResponse
	}{
		{"nil response", newBalanceReq, func(*common.NormalizedRequest) *common.NormalizedResponse { return nil }},
		{"unparseable request", covSvmBadReq, func(*common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, newBalanceReq(), `{"context":{"slot":55},"value":1}`)
		}},
		{"json-rpc error response", newBalanceReq, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmErrorResp(t, r, -32005, "Node is behind by 120 slots")
		}},
		{"zero context slot", newBalanceReq, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `{"context":{"slot":0},"value":1}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := &recordingSvmPoller{}
			req := tc.req()
			resp := tc.resp(req)
			got, err := h.HandleUpstreamPostForward(ctx, nil, &svmUpstreamStub{poller: poller}, req, resp, nil, false)
			require.NoError(t, err)
			require.Same(t, resp, got)
			require.Zero(t, poller.lastSuggested)
			require.Zero(t, poller.lastFinalizedSuggested)
		})
	}

	// An upstream without a poller yet is skipped, not dereferenced.
	req := newBalanceReq()
	resp := covSvmResultResp(t, req, `{"context":{"slot":55},"value":1}`)
	got, err := h.HandleUpstreamPostForward(ctx, nil, &svmUpstreamStub{}, req, resp, nil, false)
	require.NoError(t, err)
	require.Same(t, resp, got)
}

// A successful write passes through untouched: the non-retryable wrap applies
// only to failures.
func TestCovSvm_NonRetryableWrite_SuccessPassesThrough(t *testing.T) {
	t.Parallel()
	h := &SvmArchitectureHandler{}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["tx"]}`))
	resp := covSvmResultResp(t, req, `"5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"`)
	got, err := h.HandleUpstreamPostForward(context.Background(), nil, &svmUpstreamStub{}, req, resp, nil, false)
	require.NoError(t, err)
	require.Same(t, resp, got)
}

// ---- hooks: genesis / commitment / getBlock guard / getSlot floor ------------

func TestCovSvm_GetGenesisHash_ShortCircuitConditions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	mainnet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
	}}
	want, ok := common.KnownGenesisHash("", "mainnet-beta")
	require.True(t, ok)

	// skip-cache-read asks for the upstream's live answer.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "true"})
	handled, resp, err := h.HandleProjectPreForward(ctx, mainnet, req)
	require.NoError(t, err)
	require.False(t, handled)
	require.Nil(t, resp)

	// No svm cluster configured: nothing to answer from.
	noSvm := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req = common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	handled, resp, err = h.HandleProjectPreForward(ctx, noSvm, req)
	require.NoError(t, err)
	require.False(t, handled)
	require.Nil(t, resp)

	// A request with no id still gets a synthesized answer with an id of its own.
	req = covSvmReqFromParams("getGenesisHash", []interface{}{})
	handled, resp, err = h.HandleProjectPreForward(ctx, mainnet, req)
	require.NoError(t, err)
	require.True(t, handled)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr.ID())
	require.Equal(t, `"`+want+`"`, string(jrr.GetResultBytes()))
}

func TestCovSvm_CommitmentInjection_LeavesRequestAloneWhenNotSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	netWith := func(commitment string) *fakeNetwork {
		return &fakeNetwork{cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{Commitment: commitment},
		}}
	}

	cases := []struct {
		name       string
		commitment string
		body       string
		wantParams []interface{}
	}{
		{"invalid read default", "rooted", `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["Abc"]}`, []interface{}{"Abc"}},
		{"invalid write default", "rooted", `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["tx"]}`, []interface{}{"tx"}},
		{"non-object write config slot", "finalized", `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["tx","base64"]}`, []interface{}{"tx", "base64"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(tc.body))
			handled, resp, err := h.HandleProjectPreForward(ctx, netWith(tc.commitment), req)
			require.NoError(t, err)
			require.False(t, handled)
			require.Nil(t, resp)
			jrq, err := req.JsonRpcRequest()
			require.NoError(t, err)
			require.Equal(t, tc.wantParams, jrq.Params)
		})
	}

	// An invalid default resolves to the node's own default, never to the bogus value.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["Abc"]}`))
	require.Equal(t, solanaDefaultCommitment, effectiveCommitment(ctx, netWith("rooted"), req))
}

func TestCovSvm_GetBlockGuard_ParamShapes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{}},
		latestSlot:    1000,
		finalizedSlot: 900,
		indexedSlot:   1000,
	}

	forwarded := []struct {
		name string
		req  *common.NormalizedRequest
	}{
		{"no params", common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[]}`))},
		{"non-numeric slot", common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":["abc"]}`))},
		{"slot zero", common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[0]}`))},
		{"int slot below tip", covSvmReqFromParams("getBlock", []interface{}{int(800)})},
	}
	for _, tc := range forwarded {
		t.Run(tc.name, func(t *testing.T) {
			handled, resp, err := h.HandleNetworkPreForward(ctx, net, nil, tc.req)
			require.NoError(t, err)
			require.False(t, handled)
			require.Nil(t, resp)
		})
	}

	// Native int / int64 params (internally-built requests) are guarded too.
	for _, slot := range []interface{}{int(5000), int64(5000)} {
		handled, resp, err := h.HandleNetworkPreForward(ctx, net, nil, covSvmReqFromParams("getBlock", []interface{}{slot}))
		require.True(t, handled, "%T slot above the finalized tip must be rejected", slot)
		require.Nil(t, resp)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "got %v", err)
	}

	// A network that is not an SvmNetwork has no tips to guard with.
	handled, _, err := networkPreForward_getBlock(ctx, suffixSvmNet{id: "svm:x"}, covSvmReqFromParams("getBlock", []interface{}{int64(5000)}))
	require.NoError(t, err)
	require.False(t, handled)
}

func TestCovSvm_GetSlotFloor_PassThroughCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &SvmArchitectureHandler{}
	processedNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "processed"},
	}, latestSlot: 5000}
	finalizedColdNet := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	processedColdNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "processed"},
	}}
	newSlotReq := func() *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`))
	}

	cases := []struct {
		name string
		net  common.Network
		resp func(req *common.NormalizedRequest) *common.NormalizedResponse
	}{
		{"error body", processedNet, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmErrorResp(t, r, -32005, "Node is unhealthy")
		}},
		{"non-numeric result", processedNet, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `"abc"`)
		}},
		{"zero result", processedNet, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `0`)
		}},
		{"non-svm network", suffixSvmNet{id: "svm:x"}, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `100`)
		}},
		{"processed with cold poller", processedColdNet, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `100`)
		}},
		// Finalized with no poller tip falls back to slot-32; a slot that small
		// has no positive fallback, so the response is kept as-is.
		{"finalized cold with tiny slot", finalizedColdNet, func(r *common.NormalizedRequest) *common.NormalizedResponse {
			return covSvmResultResp(t, r, `10`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newSlotReq()
			resp := tc.resp(req)
			got, err := h.HandleNetworkPostForward(ctx, tc.net, req, resp, nil)
			require.NoError(t, err)
			require.Same(t, resp, got)
		})
	}

	// Cold finalized with a real slot: capped by the one-epoch lag fallback.
	req := newSlotReq()
	got, err := h.HandleNetworkPostForward(ctx, finalizedColdNet, req, covSvmResultResp(t, req, `1000`), nil)
	require.NoError(t, err)
	require.EqualValues(t, 968, readSlot(t, got))
}

// ---- error normalizer ----------------------------------------------------------

func TestCovSvm_Extract_EdgeInputs(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()

	// No HTTP response: nothing to classify.
	require.NoError(t, e.Extract(nil, nil, common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32005, "behind", "")), newSvmStub()))

	// A codeless, messageless auth failure still names the status and uses the
	// collision-free -32600 fallback, never -32016.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := extract(t, 0, "", status)
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "got %v", err)
		require.EqualValues(t, svmCodeInvalidRequest, wireCodeOf(t, err))
		require.Contains(t, err.Error(), "HTTP")
	}

	// -32000 with an empty message is the generic retryable server-side bucket.
	err := extract(t, -32000, "", http.StatusOK)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException), "got %v", err)
	require.EqualValues(t, -32000, wireCodeOf(t, err))
	require.False(t, isRateLimitMessage(""))
}

// ---- ledger floor / slot bounds / slot lag ---------------------------------------

func TestCovSvm_RequestRangeEndSlot_GetBlocksWithLimitMalformed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, params := range []string{`[50]`, `[50,-1]`, `[50,1.5]`} {
		end, ok := RequestRangeEndSlot(ctx, reqOf(t, "getBlocksWithLimit", params), 50, 80)
		require.False(t, ok, "params %s must fail open", params)
		require.Zero(t, end)
	}
	// Native int / int64 limits are accepted and resolve to the tip.
	for _, limit := range []interface{}{int(10), int64(10)} {
		end, ok := RequestRangeEndSlot(ctx, covSvmReqFromParams("getBlocksWithLimit", []interface{}{int64(50), limit}), 50, 80)
		require.True(t, ok)
		require.EqualValues(t, 80, end)
	}
	slot, ok := RequestLowerBoundSlot(ctx, covSvmReqFromParams("getBlock", []interface{}{int(42)}))
	require.True(t, ok)
	require.EqualValues(t, 42, slot)
}

func TestCovSvm_ResolveDeclaredBound_NoAnchor(t *testing.T) {
	t.Parallel()
	plus := int64(10)
	// firstAvailableSlotPlus without a poller has no floor to anchor to.
	require.Equal(t, int64(math.MinInt64), resolveDeclaredBound(&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: &plus}, true, nil))
	require.Equal(t, int64(math.MaxInt64), resolveDeclaredBound(&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: &plus}, false, &pollerAtSlot{null: true}))
	// A bound object with no field set is unbounded.
	require.Equal(t, int64(math.MinInt64), resolveDeclaredBound(&common.SvmAvailabilityBoundConfig{}, true, &pollerAtSlot{slot: 5}))
	require.Equal(t, int64(math.MaxInt64), resolveDeclaredBound(&common.SvmAvailabilityBoundConfig{}, false, &pollerAtSlot{slot: 5}))
}

func TestCovSvm_SlotLagFilters_NonSvmAndPollerless(t *testing.T) {
	t.Parallel()
	nonSvm := newSvmStub()
	noPoller := &slotLagUpstream{id: "nopoller"}
	nullPoller := &slotLagUpstream{id: "null", poller: &pollerAtSlot{slot: 1, null: true}}
	stale := upstreamAt("stale", 100)
	fresh := upstreamAt("fresh", 1000)

	got := FilterByFinalizedSlotLag([]common.Upstream{nonSvm, noPoller, nullPoller, stale, fresh}, 10, 1000)
	require.Equal(t, []common.Upstream{nonSvm, noPoller, nullPoller, fresh}, got,
		"upstreams that cannot be judged stay; only the stale one is dropped")

	require.EqualValues(t, 1000, HighestFinalizedSlot([]common.Upstream{nonSvm, noPoller, nullPoller, stale, fresh}))
	require.EqualValues(t, 1000, ReferenceFinalizedSlot([]common.Upstream{nonSvm, noPoller, fresh, stale}, 0))

	got = FilterByMinContextSlot([]common.Upstream{nonSvm, nullPoller, stale}, 500, true)
	require.Equal(t, []common.Upstream{nonSvm, nullPoller}, got)
}

// ---- cache ---------------------------------------------------------------------

func TestCovSvm_NewCache_ConfigErrors(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mem := &common.ConnectorConfig{Id: "mem", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 10, MaxTotalSize: "1MB"}}
	bad := "lots"

	cases := []struct {
		name    string
		cfg     *common.CacheConfig
		wantErr string
	}{
		{"unknown driver", &common.CacheConfig{Connectors: []*common.ConnectorConfig{{Id: "x", Driver: "bogus"}}}, "failed to create connector x"},
		{"policy references missing connector", &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{mem},
			Policies:   []*common.CachePolicyConfig{{Connector: "nope", Network: "*", Method: "*"}},
		}, "connector nope not found"},
		{"invalid policy size", &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{mem},
			Policies:   []*common.CachePolicyConfig{{Connector: "mem", Network: "*", Method: "*", MaxItemSize: &bad}},
		}, "failed to create policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewSvmJsonRpcCache(ctx, &log.Logger, tc.cfg)
			require.ErrorContains(t, err, tc.wantErr)
			require.Nil(t, c)
		})
	}
}

func TestCovSvm_NewCache_CompressionLevels(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	enabled := true
	blob := []byte(strings.Repeat("SolanaBlockPayload", 200))
	for _, level := range []string{"default", "Better", "best", "turbo"} {
		t.Run(level, func(t *testing.T) {
			c, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
				Compression: &common.CompressionConfig{Enabled: &enabled, ZstdLevel: level},
			})
			require.NoError(t, err)
			out := c.compress(blob)
			require.Equal(t, []byte{0x28, 0xB5, 0x2F, 0xFD}, out[:4])
			back, err := c.decompress(out)
			require.NoError(t, err)
			require.Equal(t, blob, back)
		})
	}

	// Incompressible payloads over the threshold are stored verbatim.
	c, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{
		Compression: &common.CompressionConfig{Enabled: &enabled, Threshold: 64},
	})
	require.NoError(t, err)
	random := make([]byte, 256)
	_, err = rand.Read(random)
	require.NoError(t, err)
	random[0] = 0x00 // never starts with the zstd magic
	require.Equal(t, random, c.compress(random))

	// A zstd-framed value with no decoder configured cannot be served.
	_, err = (&SvmJsonRpcCache{}).decompress([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x01})
	require.ErrorContains(t, err, "no decoder")
}

func covSvmPolicy(t *testing.T, conn data.Connector, cfg common.CachePolicyConfig) *data.CachePolicy {
	t.Helper()
	if cfg.Network == "" {
		cfg.Network = "*"
	}
	if cfg.Method == "" {
		cfg.Method = "*"
	}
	p, err := data.NewCachePolicy(&cfg, conn)
	require.NoError(t, err)
	return p
}

func covSvmCache(t *testing.T, projectId string, policies ...*data.CachePolicy) *SvmJsonRpcCache {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	base, err := NewSvmJsonRpcCache(ctx, &log.Logger, &common.CacheConfig{})
	require.NoError(t, err)
	c := base.WithProjectId(projectId)
	c.SetPolicies(policies)
	return c
}

func covSvmBlockReq(params string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":` + params + `}`))
	req.SetNetwork(finalizedNetwork{})
	return req
}

func TestCovSvm_CacheGetSet_PolicyMatchErrorsPropagate(t *testing.T) {
	t.Parallel()
	conn := data.NewMockConnector("covsvm-badpattern")
	c := covSvmCache(t, "covsvm-badpattern", covSvmPolicy(t, conn, common.CachePolicyConfig{
		Network: "(svm:*", Finality: common.DataFinalityStateFinalized, Connector: conn.Id(),
	}))
	ctx := context.Background()

	resp, err := c.Get(ctx, covSvmBlockReq(`[100]`))
	require.Error(t, err)
	require.Nil(t, resp)

	req := covSvmBlockReq(`[100]`)
	require.Error(t, c.Set(ctx, req, covSvmResultResp(t, req, `{"blockhash":"x"}`)))
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCovSvm_CacheGet_NoMatchingPolicyIsSkipped(t *testing.T) {
	t.Parallel()
	const project = "covsvm-skip"
	conn := data.NewMockConnector("covsvm-skip")
	c := covSvmCache(t, project, covSvmPolicy(t, conn, common.CachePolicyConfig{
		Method: "getAccountInfo", Finality: common.DataFinalityStateFinalized, Connector: conn.Id(),
	}))
	skipped := telemetry.MetricCacheGetSkippedTotal.WithLabelValues(project, "svm:test", "getBlock")
	before := promUtil.ToFloat64(skipped)

	resp, err := c.Get(context.Background(), covSvmBlockReq(`[100]`))
	require.NoError(t, err)
	require.Nil(t, resp)
	require.Equal(t, 1.0, promUtil.ToFloat64(skipped)-before)
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Params that cannot be encoded yield no key: Get treats it as a connector
// failure (fall through to upstream) and Set reports it.
func TestCovSvm_CacheKeyEncodingFailure(t *testing.T) {
	t.Parallel()
	conn := data.NewMockConnector("covsvm-inf")
	c := covSvmCache(t, "covsvm-inf", covSvmPolicy(t, conn, common.CachePolicyConfig{
		Finality: common.DataFinalityStateFinalized, Connector: conn.Id(),
	}))
	ctx := context.Background()
	newReq := func() *common.NormalizedRequest {
		req := covSvmReqFromParams("getBlock", []interface{}{math.Inf(1)})
		req.SetNetwork(finalizedNetwork{})
		return req
	}

	resp, err := c.Get(ctx, newReq())
	require.NoError(t, err)
	require.Nil(t, resp)

	req := newReq()
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": "x"}, nil)
	require.NoError(t, err)
	err = c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr))
	require.ErrorContains(t, err, "failed to encode svm cache key params")
	conn.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// A corrupt compressed entry is a connector error, not a hit and not a panic.
func TestCovSvm_CacheGet_CorruptCompressedEntryIsConnectorError(t *testing.T) {
	t.Parallel()
	const project = "covsvm-corrupt"
	conn := data.NewMockConnector("covsvm-corrupt")
	conn.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x01, 0x02}, nil)
	c, policies := newMissReasonCache(t, project, conn)

	beforeMiss := missCountsByReason(t, project, policies[0])
	beforeErr := cacheErrorTotalForProject(t, project)
	resp, err := c.Get(context.Background(), newMissReasonRequest(t))
	require.NoError(t, err)
	require.Nil(t, resp)
	afterMiss := missCountsByReason(t, project, policies[0])
	require.Equal(t, 1.0, afterMiss["connector_error"]-beforeMiss["connector_error"])
	require.Equal(t, 0.0, afterMiss["connector_miss"]-beforeMiss["connector_miss"])
	require.Equal(t, 1.0, cacheErrorTotalForProject(t, project)-beforeErr)
}

func TestCovSvm_CacheSet_NilAndEmptyPayloads(t *testing.T) {
	t.Parallel()
	conn := data.NewMockConnector("covsvm-empty")
	c := covSvmCache(t, "covsvm-empty", covSvmPolicy(t, conn, common.CachePolicyConfig{
		Finality: common.DataFinalityStateFinalized, Connector: conn.Id(), Empty: common.CacheEmptyBehaviorAllow,
	}))
	ctx := context.Background()

	require.NoError(t, c.Set(ctx, covSvmBlockReq(`[1]`), nil))

	// Unparseable request with a real response: the parse error is reported.
	err := c.Set(ctx, covSvmBadReq(), covSvmResultResp(t, covSvmBlockReq(`[1]`), `{"a":1}`))
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	_, err = c.Get(ctx, covSvmBadReq())
	require.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)

	// A response with an id but no result bytes has nothing to store, even
	// under a policy that accepts empty results.
	req := covSvmBlockReq(`[2]`)
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`7`), nil, nil)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)))
	conn.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestCovSvm_CacheSet_SizeLimitAndConnectorFailure(t *testing.T) {
	t.Parallel()
	const project = "covsvm-setfail"
	tiny := "4B"
	small := data.NewMockConnector("covsvm-small")
	failing := data.NewMockConnector("covsvm-failing")
	failing.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("dial tcp 10.0.0.1:6379: connect: connection refused"))
	healthy := data.NewMockConnector("covsvm-healthy")
	healthy.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	sizePolicy := covSvmPolicy(t, small, common.CachePolicyConfig{
		Finality: common.DataFinalityStateFinalized, Connector: small.Id(), MaxItemSize: &tiny,
	})
	c := covSvmCache(t, project,
		sizePolicy,
		covSvmPolicy(t, failing, common.CachePolicyConfig{Finality: common.DataFinalityStateFinalized, Connector: failing.Id()}),
		covSvmPolicy(t, healthy, common.CachePolicyConfig{Finality: common.DataFinalityStateFinalized, Connector: healthy.Id()}),
	)
	skipped := telemetry.MetricCacheSetSkippedTotal.WithLabelValues(
		project, "svm:test", "getBlock", small.Id(), sizePolicy.String(), ttlString(sizePolicy.GetTTL()),
	)
	before := promUtil.ToFloat64(skipped)

	req := covSvmBlockReq(`[3]`)
	require.NoError(t, c.Set(context.Background(), req, covSvmResultResp(t, req, `{"blockhash":"abcdef"}`)),
		"a failing connector must not fail the request path")

	require.Equal(t, 1.0, promUtil.ToFloat64(skipped)-before)
	small.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	failing.AssertNumberOfCalls(t, "Set", 1)
	healthy.AssertNumberOfCalls(t, "Set", 1)
}

func TestCovSvm_TtlString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "none", ttlString(nil))
	d := 90 * time.Second
	require.Equal(t, "1m30s", ttlString(&d))
}

func TestCovSvm_SlotRefAndNilParamsKeys(t *testing.T) {
	t.Parallel()
	c := &SvmJsonRpcCache{}
	pk := func(params []interface{}) string {
		t.Helper()
		req := covSvmReqFromParams("getAccountInfo", params)
		req.SetNetwork(finalizedNetwork{})
		rpc, err := req.JsonRpcRequest()
		require.NoError(t, err)
		g, _, err := c.generateKeys(req, rpc)
		require.NoError(t, err)
		return g
	}
	require.Equal(t, "svm:test:5", pk([]interface{}{"Abc", map[string]interface{}{"minContextSlot": int64(5)}}))
	require.Equal(t, "svm:test:77", pk([]interface{}{"Abc", map[string]interface{}{"minContextSlot": "77"}}))
	require.Equal(t, "svm:test:*", pk([]interface{}{"Abc", map[string]interface{}{"minContextSlot": ""}}))
	require.Equal(t, "*", extractSlotRef(nil))

	nilKey, err := svmRequestKey(common.NewJsonRpcRequest("getSlot", nil))
	require.NoError(t, err)
	emptyKey, err := svmRequestKey(common.NewJsonRpcRequest("getSlot", []interface{}{}))
	require.NoError(t, err)
	require.Equal(t, emptyKey, nilKey, "absent and empty params are the same call")
}

// ---- state poller ----------------------------------------------------------------

// covSvmFuncUpstream lets a poller test decide each response in code: block,
// panic, or return a nil response.
type covSvmFuncUpstream struct {
	fakeUpstreamForPoller
	id  string
	cfg *common.UpstreamConfig
	fwd func(ctx context.Context, req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error)
}

func (u *covSvmFuncUpstream) Id() string { return u.id }
func (u *covSvmFuncUpstream) Config() *common.UpstreamConfig {
	if u.cfg != nil {
		return u.cfg
	}
	return &common.UpstreamConfig{Id: u.id, Type: common.UpstreamTypeSvm, Endpoint: "http://" + u.id}
}
func (u *covSvmFuncUpstream) Forward(ctx context.Context, req *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	return u.fwd(ctx, req, requestKey(string(req.Body())))
}

func covSvmHealthyAnswer(req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error) {
	raw := map[string]string{
		"getHealth":             `"ok"`,
		"getSlot:processed":     `1000`,
		"getSlot:finalized":     `990`,
		"getMaxShredInsertSlot": `1001`,
		"oldestBlock":           `[200]`,
	}[key]
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(raw), nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

func TestCovSvmPoller_MalformedSlotAnswersAreIgnored(t *testing.T) {
	t.Parallel()
	up := newScriptedUpstream()
	up.script("getHealth", []byte(`"ok"`))
	up.script("getSlot:processed", []byte(`null`))
	up.scriptError("getSlot:finalized", -32005, "Node is behind by 42 slots")
	up.script("getMaxShredInsertSlot", []byte(`"abc"`))
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, p.LatestSlot())
	require.Zero(t, p.FinalizedSlot())
	require.Zero(t, p.ShredInsertSlot())
	require.True(t, p.IsHealthy(), "a bad slot sample is not a health verdict")
}

func TestCovSvmPoller_NilHealthResponseIsUnhealthy(t *testing.T) {
	t.Parallel()
	up := &covSvmFuncUpstream{id: "covsvm-nilhealth", fwd: func(_ context.Context, req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error) {
		if key == "getHealth" {
			return nil, nil
		}
		return covSvmHealthyAnswer(req, key)
	}}
	p := newPollerWithUpstream(t, up)
	require.NoError(t, p.Poll(context.Background()))
	require.False(t, p.IsHealthy())
	require.EqualValues(t, 1000, p.LatestSlot())
}

func TestCovSvmPoller_PanickingUpstreamIsRecovered(t *testing.T) {
	t.Parallel()
	var panicking atomic.Bool
	panicking.Store(true)
	// The floor probe runs too; its panic is recovered inside the refresh closure.
	up := &covSvmFuncUpstream{id: "covsvm-panic",
		fwd: func(_ context.Context, req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error) {
			if panicking.Load() {
				panic("transport exploded")
			}
			return covSvmHealthyAnswer(req, key)
		}}
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	require.Zero(t, p.LatestSlot())

	// The poller keeps working once the upstream recovers.
	panicking.Store(false)
	require.NoError(t, p.Poll(context.Background()))
	require.EqualValues(t, 1000, p.LatestSlot())
	require.EqualValues(t, 990, p.FinalizedSlot())
}

func TestCovSvmPoller_FloorProbeFailuresLeaveFloorUnknown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		script func(up *scriptedUpstream)
	}{
		{"negative slot", func(up *scriptedUpstream) { up.script("oldestBlock", []byte(`[-5]`)) }},
		{"unparseable result", func(up *scriptedUpstream) { up.script("oldestBlock", []byte(`"abc"`)) }},
		{"transport error", func(*scriptedUpstream) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newScriptedUpstream()
			scriptAllFour(up)
			tc.script(up)
			p := newPollerWithUpstream(t, up)
			require.NoError(t, p.Poll(context.Background()))
			_, known := p.FirstAvailableSlot()
			require.False(t, known)
			require.Equal(t, 1, up.callCount("oldestBlock"))
			require.EqualValues(t, 1000, p.LatestSlot())
		})
	}
}

// A floor probe slower than UpdateMaxWait returns without a value; the floor
// becomes known once the background fetch lands.
func TestCovSvmPoller_SlowFloorProbeCompletesAsync(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	up := &covSvmFuncUpstream{id: "covsvm-slowfloor",
		fwd: func(ctx context.Context, req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error) {
			if key == "oldestBlock" {
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return covSvmHealthyAnswer(req, key)
		}}
	p := newPollerWithUpstream(t, up)

	require.NoError(t, p.Poll(context.Background()))
	_, known := p.FirstAvailableSlot()
	require.False(t, known, "the floor must not be known before the probe answers")

	close(release)
	require.Eventually(t, func() bool {
		floor, known := p.FirstAvailableSlot()
		return known && floor == 200
	}, 5*time.Second, 5*time.Millisecond)
}

func TestCovSvmPoller_LargeRollbackIsRecorded(t *testing.T) {
	t.Parallel()
	const id = "covsvm-rollback"
	up := &covSvmFuncUpstream{id: id, fwd: func(_ context.Context, req *common.NormalizedRequest, key string) (*common.NormalizedResponse, error) {
		return covSvmHealthyAnswer(req, key)
	}}
	p := newPollerWithUpstream(t, up)

	p.SuggestLatestSlot(5000)
	p.SuggestLatestSlot(1000) // 4000 back: beyond DefaultToleratedSlotRollback
	require.EqualValues(t, 1000, p.LatestSlot(), "a large rollback is applied, not ignored")
	require.Equal(t, 4000.0, promUtil.ToFloat64(
		telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues("test", "", "", id, "latest")))

	p.SuggestFinalizedSlot(3000)
	p.SuggestFinalizedSlot(100)
	require.EqualValues(t, 100, p.FinalizedSlot())
	require.Equal(t, 2900.0, promUtil.ToFloat64(
		telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues("test", "", "", id, "finalized")))
}

func TestCovSvmPoller_NonPositiveSuggestionsIgnored(t *testing.T) {
	t.Parallel()
	p := newTestPoller(t)
	p.suggestLatestSlot(0)
	p.suggestFinalizedSlot(-1)
	p.SuggestFinalizedSlot(0)
	require.Zero(t, p.LatestSlot())
	require.Zero(t, p.FinalizedSlot())
	require.Zero(t, p.lastExternalFinalizedAt.Load(), "a rejected suggestion is not freshness evidence")
}

func TestCovSvmPoller_NoUpstream(t *testing.T) {
	t.Parallel()
	e := &SvmStatePoller{projectId: "covsvm-noupstream"}
	e.applyHealthToRouting() // nothing to cordon; must return before touching upstream
	e.gauge(telemetry.MetricUpstreamSvmFirstAvailableSlot).Set(7)
	require.Equal(t, 7.0, promUtil.ToFloat64(
		telemetry.MetricUpstreamSvmFirstAvailableSlot.WithLabelValues("covsvm-noupstream", "", "", "")))
}
