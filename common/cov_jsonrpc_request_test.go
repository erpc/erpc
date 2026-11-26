package common

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCovJsonrpcRequest_ContextMarkers(t *testing.T) {
	ctx := context.Background()
	assert.False(t, IsConsensusSlot(ctx))
	assert.True(t, IsConsensusSlot(WithConsensusSlot(ctx)))
	assert.False(t, IsSweepIteration(ctx))
	assert.True(t, IsSweepIteration(WithSweepIteration(ctx)))

	a := NewNormalizedRequest(nil)
	b := NewNormalizedRequest(nil)
	own := []interface{}{"own"}
	over := []interface{}{"override"}
	wctx := WithWireParams(ctx, a, over)
	assert.Equal(t, over, WireParams(wctx, a, own), "override applies to its own request")
	assert.Equal(t, own, WireParams(wctx, b, own), "override never leaks to other requests")
	assert.Equal(t, own, WireParams(ctx, a, own))
}

func TestCovJsonrpcRequest_DirectivesClone(t *testing.T) {
	var nilD *RequestDirectives
	assert.Equal(t, &RequestDirectives{}, nilD.Clone())

	d := &RequestDirectives{
		RetryEmpty: true, RetryPending: true, SkipCacheRead: "redis*", UseUpstream: "a*",
		ByPassMethodExclusion: true, SkipInterpolation: true, SkipConsensus: true,
		EnforceHighestBlock: true, EnforceGetLogsBlockRange: true, EnforceNonNullTaggedBlocks: true,
		IntegritySelector: "strict",
	}
	c := d.Clone()
	assert.Equal(t, d, c)
	assert.NotSame(t, d, c)
}

func TestCovJsonrpcRequest_NilReceiver(t *testing.T) {
	var r *NormalizedRequest
	ctx := context.Background()
	assert.Nil(t, r.ExecState())
	assert.Zero(t, r.CreditUnitsTotal())
	assert.Nil(t, r.CreditUnitsByVendor())
	assert.Equal(t, UpstreamPoliciesConfigured, r.UpstreamPolicies())
	r.SetUser(&User{Id: "x"})
	r.SetUserFromTrustedHeader("x")
	assert.Nil(t, r.User())
	assert.Nil(t, r.SetLastUpstream(&covJsonrpcUpstream{id: "a"}))
	assert.Nil(t, r.LastUpstream())
	assert.False(t, r.SetLastValidResponse(ctx, NewNormalizedResponse()))
	assert.Nil(t, r.LastValidResponse())
	r.ClearLastValidResponse()
	r.ClearLastValidResponseIf(NewNormalizedResponse())
	r.MarkIntegrityCaught("c", "f")
	assert.False(t, r.IntegrityCaught())
	r.SetIntegrityFallbackResponse(NewNormalizedResponse(), "c", "f", "r")
	assert.Nil(t, r.TakeIntegrityFallbackResponse())
	assert.Equal(t, "", r.IntegrityRejectedCheck())
	assert.Equal(t, "", r.IntegrityRejectedFinality())
	r.AddIntegrityOverhead(time.Second)
	assert.Zero(t, r.IntegrityOverhead())
	assert.Nil(t, r.Network())
	assert.Nil(t, r.ID())
	assert.Equal(t, "n/a", r.NetworkId())
	assert.Equal(t, "", r.CacheKeySuffix())
	assert.Equal(t, "n/a", r.NetworkLabel())
	assert.Nil(t, r.CacheDal())
	r.SetDirectives(&RequestDirectives{})
	r.SetAllowClientDirectiveMatcher(nil)
	assert.False(t, r.ShouldSkipCacheRead("x"))
	assert.Nil(t, r.Directives())
	jrq, err := r.JsonRpcRequest()
	assert.Nil(t, jrq)
	assert.NoError(t, err)
	m, err := r.Method()
	assert.Equal(t, "", m)
	assert.NoError(t, err)
	assert.Equal(t, "", r.NetworkIdHint())
	r.SetClientIP("1.2.3.4")
	assert.Equal(t, "n/a", r.ClientIP())
	r.SetAgentName("x")
	assert.Nil(t, r.EvmBlockRef())
	r.SetEvmBlockRef("x")
	assert.Nil(t, r.EvmBlockNumber())
	r.SetEvmBlockNumber(int64(1))
	assert.True(t, HasErrorCode(r.Validate(), ErrCodeInvalidRequest))
	assert.False(t, r.IsCompositeRequest())
	assert.Equal(t, "", r.CompositeType())
	r.SetCompositeType("x")
	assert.Nil(t, r.ParentRequestId())
	r.SetParentRequestId(1)
	assert.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
	r.CopyHttpContextFrom(NewNormalizedRequest(nil))
	r.SetUpstreams(nil)
	assert.Zero(t, r.UpstreamsCount())
	assert.Nil(t, r.Upstreams())
	assert.False(t, r.MarkEscalatedToFallbacks())
	assert.Equal(t, "n/a", r.UserId())
	assert.Equal(t, "unknown", r.AgentName())
	_, err = r.NextUpstream()
	assert.ErrorContains(t, err, "uninitialized request")
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	r.MarshalZerologObject(lg.Info())
}

func TestCovJsonrpcRequest_ExecStateAndCredits(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"method":"eth_call"}`))
	st := r.ExecState()
	require.NotNil(t, st)
	assert.Same(t, st, r.ExecState(), "exec state is created once")
	assert.Zero(t, r.CreditUnitsTotal())
	assert.Nil(t, r.CreditUnitsByVendor())
}

func TestCovJsonrpcRequest_UpstreamPolicies(t *testing.T) {
	r := NewNormalizedRequest(nil)
	assert.Equal(t, UpstreamPoliciesConfigured, r.UpstreamPolicies(), "no directives")
	r.SetDirectives(&RequestDirectives{UpstreamPolicies: UpstreamPoliciesTracker})
	assert.Equal(t, UpstreamPoliciesTracker, r.UpstreamPolicies())
}

func TestCovJsonrpcRequest_UserAndUpstreamAccessors(t *testing.T) {
	r := NewNormalizedRequest(nil)
	r.SetUser(nil)
	assert.Nil(t, r.User())
	assert.Equal(t, "n/a", r.UserId())
	r.SetUser(&User{})
	assert.Equal(t, "n/a", r.UserId(), "empty user id reported as n/a")
	r.SetUser(&User{Id: "alice"})
	assert.Equal(t, "alice", r.UserId())

	assert.Nil(t, r.LastUpstream())
	r.SetLastUpstream(nil)
	assert.Nil(t, r.LastUpstream())
	up := &covJsonrpcUpstream{id: "u1", cfg: &UpstreamConfig{Id: "u1"}}
	assert.Same(t, r, r.SetLastUpstream(up))
	assert.Same(t, up, r.LastUpstream())

	assert.Equal(t, "n/a", r.ClientIP())
	r.SetClientIP("")
	assert.Equal(t, "n/a", r.ClientIP())
	r.SetClientIP("10.0.0.1")
	assert.Equal(t, "10.0.0.1", r.ClientIP())

	r.SetAgentName("")
	assert.Equal(t, "unknown", r.AgentName())
	r.SetAgentName("my-bot")
	assert.Equal(t, "my-bot", r.AgentName())

	r.SetEvmBlockRef(nil)
	assert.Nil(t, r.EvmBlockRef())
	r.SetEvmBlockRef("latest")
	assert.Equal(t, "latest", r.EvmBlockRef())
	r.SetEvmBlockNumber(nil)
	assert.Nil(t, r.EvmBlockNumber())
	r.SetEvmBlockNumber(int64(100))
	assert.Equal(t, int64(100), r.EvmBlockNumber())

	assert.False(t, r.IsCompositeRequest())
	r.SetCompositeType("")
	assert.Equal(t, CompositeTypeNone, r.CompositeType())
	r.SetCompositeType(CompositeTypeLogsSplitOnError)
	assert.True(t, r.IsCompositeRequest())
	assert.Equal(t, "", (&NormalizedRequest{}).CompositeType(), "zero-value request has no composite type")

	assert.Nil(t, r.ParentRequestId())
	r.SetParentRequestId(nil)
	assert.Nil(t, r.ParentRequestId())
	r.SetParentRequestId("parent-1")
	assert.Equal(t, "parent-1", r.ParentRequestId())

	assert.True(t, r.MarkEscalatedToFallbacks())
	assert.False(t, r.MarkEscalatedToFallbacks(), "escalation happens at most once")

	r.SetUpstreams(nil)
	assert.Nil(t, r.Upstreams())
	ups := []Upstream{up, &covJsonrpcUpstream{id: "u2"}}
	r.SetUpstreams(ups)
	assert.Equal(t, 2, r.UpstreamsCount())
	snap := r.Upstreams()
	snap[0] = nil
	assert.Same(t, up, r.Upstreams()[0], "snapshot is a copy")

	var cd CacheDAL
	r.SetCacheDal(cd)
	assert.Nil(t, r.CacheDal())
}

func TestCovJsonrpcRequest_IntegrityTracking(t *testing.T) {
	r := NewNormalizedRequest(nil)
	assert.False(t, r.IntegrityCaught())
	assert.Equal(t, "", r.IntegrityRejectedCheck())
	assert.Equal(t, "", r.IntegrityRejectedFinality())

	r.MarkIntegrityCaught("", "")
	assert.True(t, r.IntegrityCaught())
	assert.Equal(t, "", r.IntegrityRejectedCheck(), "empty check id is not recorded")

	r.MarkIntegrityCaught("receipts-root", "finalized")
	assert.Equal(t, "receipts-root", r.IntegrityRejectedCheck())
	assert.Equal(t, "finalized", r.IntegrityRejectedFinality())

	r.AddIntegrityOverhead(0)
	r.AddIntegrityOverhead(-time.Second)
	assert.Zero(t, r.IntegrityOverhead(), "non-positive durations ignored")
	r.AddIntegrityOverhead(2 * time.Millisecond)
	r.AddIntegrityOverhead(3 * time.Millisecond)
	assert.Equal(t, 5*time.Millisecond, r.IntegrityOverhead())

	r.SetIntegrityFallbackResponse(nil, "c", "f", "r")
	assert.Nil(t, r.TakeIntegrityFallbackResponse())
}

func TestCovJsonrpcRequest_LastValidResponseLifecycle(t *testing.T) {
	ctx := context.Background()
	r := NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","id":1}`))
	assert.False(t, r.SetLastValidResponse(ctx, nil))

	up := &covJsonrpcUpstream{id: "u", cfg: &UpstreamConfig{Id: "u"}}
	empty := NewNormalizedResponse().SetUpstream(up).WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`null`), nil))
	full := NewNormalizedResponse().SetUpstream(up).WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"n":"0x1"}`), nil))
	empty2 := NewNormalizedResponse().WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`[]`), nil))

	assert.True(t, r.SetLastValidResponse(ctx, empty), "empty accepted when slot empty")
	assert.Same(t, up, r.LastUpstream())
	assert.True(t, r.SetLastValidResponse(ctx, full), "non-empty replaces empty")
	assert.False(t, r.SetLastValidResponse(ctx, empty2), "empty never replaces non-empty")
	assert.Same(t, full, r.LastValidResponse())

	r.ClearLastValidResponseIf(nil)
	r.ClearLastValidResponseIf(empty2)
	assert.Same(t, full, r.LastValidResponse(), "conditional clear only drops matching response")
	r.ClearLastValidResponseIf(full)
	assert.Nil(t, r.LastValidResponse())

	r.SetLastValidResponse(ctx, full)
	r.ClearLastValidResponse()
	assert.Nil(t, r.LastValidResponse())
}

func TestCovJsonrpcRequest_NetworkAccessors(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"method":"eth_chainId","id":1}`))
	assert.Equal(t, "n/a", r.NetworkId())
	assert.Equal(t, "n/a", r.NetworkLabel())
	assert.Equal(t, "", r.CacheKeySuffix())

	nw := &covJsonrpcNetwork{id: "evm:1"}
	r.SetNetwork(nw)
	assert.Same(t, nw, r.Network())
	assert.Equal(t, "evm:1", r.NetworkId())
	assert.Equal(t, "evm:1", r.NetworkLabel(), "label falls back to id")
	assert.Equal(t, "", r.CacheKeySuffix(), "nil config yields no suffix")

	nw.label = "mainnet"
	nw.cfg = &NetworkConfig{CacheKeySuffix: "v2"}
	assert.Equal(t, "mainnet", r.NetworkLabel())
	assert.Equal(t, "v2", r.CacheKeySuffix())
}

func TestCovJsonrpcRequest_Finality(t *testing.T) {
	ctx := context.Background()
	r := NewNormalizedRequest([]byte(`{"method":"eth_call"}`))
	assert.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))

	nw := &covJsonrpcNetwork{id: "evm:1", finality: DataFinalityStateUnknown}
	r.SetNetwork(nw)
	assert.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
	assert.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
	assert.Equal(t, 2, nw.finalityCalls, "unknown is not cached")

	nw.finality = DataFinalityStateUnfinalized
	assert.Equal(t, DataFinalityStateUnfinalized, r.Finality(ctx))
	nw.finality = DataFinalityStateFinalized
	assert.Equal(t, DataFinalityStateUnfinalized, r.Finality(ctx), "definitive finality is cached")
}

func TestCovJsonrpcRequest_ApplyDirectiveDefaults(t *testing.T) {
	r := NewNormalizedRequest(nil)
	r.ApplyDirectiveDefaults(nil)
	assert.Nil(t, r.Directives(), "nil defaults are a no-op")

	up := "primary-*"
	r.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{
		RetryEmpty:                 util.BoolPtr(true),
		RetryPending:               util.BoolPtr(true),
		SkipCacheRead:              true,
		UseUpstream:                &up,
		SkipInterpolation:          util.BoolPtr(true),
		SkipConsensus:              util.BoolPtr(true),
		EnforceHighestBlock:        util.BoolPtr(true),
		EnforceGetLogsBlockRange:   util.BoolPtr(true),
		EnforceNonNullTaggedBlocks: util.BoolPtr(true),
	})
	d := r.Directives()
	assert.Equal(t, &RequestDirectives{
		RetryEmpty: true, RetryPending: true, SkipCacheRead: "true", UseUpstream: "primary-*",
		SkipInterpolation: true, SkipConsensus: true, EnforceHighestBlock: true,
		EnforceGetLogsBlockRange: true, EnforceNonNullTaggedBlocks: true,
	}, d)

	r.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)})
	assert.True(t, r.Directives().RetryEmpty, "defaults never overwrite populated directives")

	r2 := NewNormalizedRequest(nil)
	r2.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheRead: "redis*"})
	assert.Equal(t, "redis*", r2.Directives().SkipCacheRead)
}

func TestCovJsonrpcRequest_EnrichFromHttp_AllDirectives(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-ERPC-Retry-Empty", " TRUE ")
	headers.Set("X-ERPC-Retry-Pending", "false")
	headers.Set("X-ERPC-Skip-Cache-Read", " memory* ")
	headers.Set("X-ERPC-Use-Upstream", "alchemy")
	headers.Set("X-ERPC-Skip-Interpolation", "true")
	headers.Set("X-ERPC-Skip-Consensus", "true")
	headers.Set("X-ERPC-Enforce-Highest-Block", "true")
	headers.Set("X-ERPC-Enforce-GetLogs-Range", "true")
	headers.Set("X-ERPC-Enforce-Non-Null-Tagged-Blocks", "true")
	headers.Set("X-ERPC-Integrity", " strict ")
	headers.Set("User-Agent", "Mozilla/5.0 Chrome/120")

	r := NewNormalizedRequest(nil)
	r.EnrichFromHttp(headers, nil, UserAgentTrackingModeSimplified)
	assert.Equal(t, &RequestDirectives{
		RetryEmpty: true, RetryPending: false, SkipCacheRead: "memory*", UseUpstream: "alchemy",
		SkipInterpolation: true, SkipConsensus: true, EnforceHighestBlock: true,
		EnforceGetLogsBlockRange: true, EnforceNonNullTaggedBlocks: true, IntegritySelector: "strict",
	}, r.Directives())
	assert.Equal(t, "chrome", r.AgentName())

	// Query params override headers.
	q := url.Values{}
	q.Set("retry-empty", "false")
	q.Set("retry-pending", "true")
	q.Set("skip-cache-read", "true")
	q.Set("use-upstream", " infura ")
	q.Set("skip-interpolation", "false")
	q.Set("skip-consensus", "false")
	q.Set("enforce-highest-block", "false")
	q.Set("enforce-getlogs-range", "false")
	q.Set("enforce-non-null-tagged-blocks", "false")
	q.Set("integrity", "off")
	q.Set("user-agent", "my-custom-agent/1.2")
	r.EnrichFromHttp(headers, q, UserAgentTrackingModeRaw)
	assert.Equal(t, &RequestDirectives{
		RetryEmpty: false, RetryPending: true, SkipCacheRead: "true", UseUpstream: "infura",
		IntegritySelector: "off",
	}, r.Directives())
	assert.Equal(t, "my-custom-agent/1.2", r.AgentName(), "raw mode keeps the agent verbatim")

	// No directives: only the agent is captured, directives untouched.
	r2 := NewNormalizedRequest(nil)
	r2.EnrichFromHttp(http.Header{"User-Agent": []string{"curl/8"}}, nil, UserAgentTrackingModeSimplified)
	assert.Nil(t, r2.Directives())
	assert.Equal(t, "curl", r2.AgentName())

	r3 := NewNormalizedRequest(nil)
	r3.EnrichFromHttp(nil, nil, UserAgentTrackingModeSimplified)
	assert.Equal(t, "unknown", r3.AgentName())

	// Copy-on-write: shared defaults are not mutated.
	shared := &RequestDirectives{RetryPending: true}
	r4 := NewNormalizedRequest(nil)
	r4.SetDirectives(shared)
	r4.EnrichFromHttp(http.Header{"X-Erpc-Retry-Pending": []string{"false"}}, nil, UserAgentTrackingModeSimplified)
	assert.True(t, shared.RetryPending)
	assert.False(t, r4.Directives().RetryPending)
}

func TestCovJsonrpcRequest_DirectiveMatcherEmptyKey(t *testing.T) {
	r := NewNormalizedRequest(nil)
	r.SetAllowClientDirectiveMatcher(func(string) bool { return true })
	assert.False(t, r.isDirectiveAllowed(""), "unknown directive keys are never allowed under a matcher")
	assert.True(t, r.isDirectiveAllowed("retry-empty"))
	assert.False(t, hasDirectiveInHeaders(nil))
	assert.False(t, hasDirectiveInQueryParams(nil))
}

func TestCovJsonrpcRequest_ShouldSkipCacheRead(t *testing.T) {
	cases := []struct {
		directive string
		connector string
		want      bool
	}{
		{"", "redis-1", false},
		{"false", "redis-1", false},
		{"FALSE", "redis-1", false},
		{"True", "", true},
		{"redis*", "", false},
		{"redis*", "redis-1", true},
		{"redis*", "memory-1", false},
		{"memory*|dynamo*", "dynamo-x", true},
	}
	for _, tc := range cases {
		r := NewNormalizedRequest(nil)
		r.SetDirectives(&RequestDirectives{SkipCacheRead: tc.directive})
		assert.Equal(t, tc.want, r.ShouldSkipCacheRead(tc.connector), "%q vs %q", tc.directive, tc.connector)
	}
	assert.False(t, NewNormalizedRequest(nil).ShouldSkipCacheRead("x"), "no directives")
}

func TestCovJsonrpcRequest_MethodAndParsing(t *testing.T) {
	ctx := context.Background()

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","id":"abc","networkId":"evm:10"}`))
	assert.Equal(t, "evm:10", r.NetworkIdHint())
	m, err := r.Method()
	require.NoError(t, err)
	assert.Equal(t, "eth_getBalance", m)
	assert.Equal(t, "abc", r.ID())
	assert.Equal(t, "evm:10", r.NetworkIdHint(), "hint read from cached envelope")

	_, err = NewNormalizedRequest(nil).Method()
	assert.True(t, HasErrorCode(err, "ErrJsonRpcRequestUnresolvableMethod"), "got %v", err)

	_, err = NewNormalizedRequest([]byte(`{bad`)).Method()
	assert.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal), "got %v", err)

	_, err = NewNormalizedRequest([]byte(`{"method":""}`)).JsonRpcRequest(ctx)
	assert.True(t, HasErrorCode(err, "ErrJsonRpcRequestUnresolvableMethod"), "got %v", err)

	assert.Equal(t, "", NewNormalizedRequest([]byte(`{bad`)).NetworkIdHint())
	assert.Equal(t, "", NewNormalizedRequest(nil).NetworkIdHint())
	assert.Nil(t, NewNormalizedRequest([]byte(`{bad`)).ID())

	// Body is retained until JsonRpcRequest consumes it.
	r2 := NewNormalizedRequest([]byte(`{"method":"eth_call","id":1}`))
	assert.NotNil(t, r2.Body())
	_, err = r2.JsonRpcRequest(ctx)
	require.NoError(t, err)
	assert.Nil(t, r2.Body())
}

func TestCovJsonrpcRequest_Validate(t *testing.T) {
	assert.True(t, HasErrorCode(NewNormalizedRequest(nil).Validate(), ErrCodeInvalidRequest))
	err := NewNormalizedRequest([]byte(`{bad`)).Validate()
	assert.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
	assert.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal))

	empty := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{Method: ""})
	err = empty.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method is required")

	assert.NoError(t, NewNormalizedRequest([]byte(`{"method":"eth_call"}`)).Validate())
}

func TestCovJsonrpcRequest_MarshalJSON(t *testing.T) {
	body := []byte(`{"method":"eth_call","id":1}`)
	b, err := NewNormalizedRequest(body).MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, body, b)

	fromJrq := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest("eth_chainId", nil))
	b, err = fromJrq.MarshalJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"method":"eth_chainId"}`, string(b))

	b, err = (&NormalizedRequest{}).MarshalJSON()
	assert.Nil(t, b)
	assert.NoError(t, err)
}

func TestCovJsonrpcRequest_CacheHash(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0xAbC","latest"],"id":1}`))
	h, err := r.CacheHash(context.Background())
	require.NoError(t, err)
	r2 := NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0xabc","latest"],"id":2}`))
	h2, err := r2.CacheHash()
	require.NoError(t, err)
	assert.Equal(t, h, h2, "id and hex case do not affect cache hash")

	_, err = NewNormalizedRequest([]byte(`{bad`)).CacheHash()
	assert.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal))

	var nilR *NormalizedRequest
	_, err = nilR.CacheHash()
	assert.ErrorContains(t, err, "not valid to generate cache hash")
}

func TestCovJsonrpcNormalizedRequest_MarshalZerologObject(t *testing.T) {
	covJsonrpcEnableLogs(t)
	render := func(r *NormalizedRequest) string {
		var buf bytes.Buffer
		lg := zerolog.New(&buf)
		lg.Info().Object("rq", r).Send()
		return buf.String()
	}

	out := render(NewNormalizedRequest([]byte(`{"method":"m"}`)))
	assert.Contains(t, out, `"lastUpstream":"nil"`)
	assert.Contains(t, out, `"body":{"method":"m"}`)

	out = render(NewNormalizedRequest([]byte(`garbage`)))
	assert.Contains(t, out, `"body":"garbage"`)

	r := NewNormalizedRequestFromJsonRpcRequest(NewJsonRpcRequest("eth_call", nil))
	r.SetNetwork(&covJsonrpcNetwork{id: "evm:137"})
	r.SetLastUpstream(&covJsonrpcUpstream{id: "u", cfg: &UpstreamConfig{Id: "u"}})
	out = render(r)
	assert.Contains(t, out, `"lastUpstream":"u"`)
	assert.Contains(t, out, `"networkId":"evm:137"`)
	assert.Contains(t, out, `"jsonRpc":{"method":"eth_call"`)

	r.SetLastUpstream(&covJsonrpcUpstream{id: "nocfg"})
	assert.Regexp(t, `"lastUpstream":"0x[0-9a-f]+"`, render(r))
}

func TestCovJsonrpcRequest_CopyHttpContextFrom(t *testing.T) {
	src := NewNormalizedRequest(nil)
	src.SetAgentName("viem")
	src.SetUser(&User{Id: "bob"})
	dst := NewNormalizedRequest(nil)
	dst.CopyHttpContextFrom(nil)
	assert.Equal(t, "unknown", dst.AgentName())
	dst.CopyHttpContextFrom(src)
	assert.Equal(t, "viem", dst.AgentName())
	assert.Equal(t, "bob", dst.UserId())

	dst2 := NewNormalizedRequest(nil)
	dst2.CopyHttpContextFrom(NewNormalizedRequest(nil))
	assert.Equal(t, "unknown", dst2.AgentName())
	assert.Nil(t, dst2.User())
}

func TestCovJsonrpcRequest_SimplifyAgentName(t *testing.T) {
	r := NewNormalizedRequest(nil)
	cases := map[string]string{
		"curl/8.1":                       "curl",
		"Wget/1.21":                      "wget",
		"PostmanRuntime/7":               "postman",
		"insomnia/2023":                  "insomnia",
		"HTTPie/3":                       "httpie",
		"python-requests/2.31":           "python",
		"node-fetch/1.0":                 "nodejs",
		"javascript-client":              "nodejs",
		"Go-http-client/1.1":             "go",
		"go/1.22":                        "go",
		"Java/17":                        "java",
		"rust-reqwest":                   "rust",
		"Ruby":                           "ruby",
		"PHP/8":                          "php",
		"Mozilla Chrome/1":               "chrome",
		"Mozilla Firefox/1":              "firefox",
		"Mozilla Safari/1":               "safari",
		"Mozilla Edge/1":                 "edge",
		"viem/2.0":                       "viem",
		"ethers/6":                       "ethers",
		"web3.py":                        "web3",
		"web3j-client":                   "web3",
		"alchemy-sdk":                    "alchemy-sdk",
		"infura-client":                  "infura-sdk",
		"quicknode-x":                    "nodejs", // "node" substring wins over quicknode
		"QuickNode-SDK/1":                "nodejs",
		"MyBot/1.0 (+https://x)":         "mybot",
		"averyveryverylongagentname-xyz": "averyveryverylongag"[:19] + "e",
		"(weird)":                        "other",
		"   ":                            "other",
	}
	for in, want := range cases {
		assert.Equal(t, want, r.simplifyAgentName(in), "agent %q", in)
	}
}

func TestCovJsonrpcRequest_NextUpstream_UseUpstreamAndGates(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"method":"eth_call","id":1}`))
	_, err := r.NextUpstream()
	assert.True(t, HasErrorCode(err, ErrCodeNoUpstreamsLeftToSelect))

	a := &covJsonrpcUpstream{id: "alchemy", cfg: &UpstreamConfig{Id: "alchemy"}}
	b := &covJsonrpcUpstream{id: "infura", cfg: &UpstreamConfig{Id: "infura", Tags: []string{"family:systx"}}}
	r.SetUpstreams([]Upstream{a, b})
	r.SetDirectives(&RequestDirectives{UseUpstream: "family:systx"})

	got, err := r.NextUpstream()
	require.NoError(t, err)
	assert.Same(t, b, got, "selector matches upstream tags")
	_, err = r.NextUpstream()
	assert.True(t, HasErrorCode(err, ErrCodeNoUpstreamsLeftToSelect), "non-matching upstreams are never picked")
}

func TestCovJsonrpcRequest_MarkUpstreamCompleted(t *testing.T) {
	ctx := context.Background()
	up := &covJsonrpcUpstream{id: "u", cfg: &UpstreamConfig{Id: "u"}}

	t.Run("canceled hedge frees reservation without recording", func(t *testing.T) {
		r := NewNormalizedRequest([]byte(`{"method":"eth_call","id":1}`))
		r.SetUpstreams([]Upstream{up})
		_, err := r.NextUpstream()
		require.NoError(t, err)
		r.MarkUpstreamCompleted(ctx, up, nil, NewErrEndpointRequestCanceled(errors.New("hedge won")))
		_, recorded := r.ErrorsByUpstream.Load(up)
		assert.False(t, recorded)
		again, err := r.NextUpstream()
		require.NoError(t, err)
		assert.Same(t, up, again)
	})

	t.Run("emptyish without parsed json-rpc records missing data", func(t *testing.T) {
		r := NewNormalizedRequest([]byte(`{"method":"eth_call","id":1}`))
		r.SetUpstreams([]Upstream{up})
		_, err := r.NextUpstream()
		require.NoError(t, err)
		// A response whose body never parses still exposes a json-rpc error object;
		// an emptyish result is recorded as missing data.
		resp := NewNormalizedResponse().WithJsonRpcResponse(MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x"`), nil))
		r.MarkUpstreamCompleted(ctx, up, resp, nil)
		v, ok := r.ErrorsByUpstream.Load(up)
		require.True(t, ok)
		assert.True(t, HasErrorCode(v.(error), ErrCodeEndpointMissingData))
		assert.Contains(t, v.(error).Error(), "emptyish")
	})
}
