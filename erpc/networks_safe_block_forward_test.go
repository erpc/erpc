package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── end-to-end: what actually leaves the proxy ──────────────────────────────
//
// The tests above pin the resolver and the normalizer in isolation. These two
// drive the real request path with real state pollers over mocked HTTP, because
// the contract that matters to a consumer is the BYTES an upstream receives.

// clientRequestBodies records, per upstream host, the JSON-RPC body the client's
// request produced on the wire.
//
// The recorder's gock filter selects on the method plus `includeTransactions:
// true`: the state poller's own probes always pass `false`, so that flag cleanly
// separates the client's request from background polling. The filter
// deliberately says nothing about the block parameter — that is what the test
// asserts, so the assertion cannot be a restatement of the matcher.
type clientRequestBodies struct {
	mu     sync.Mutex
	byHost map[string]string
}

func newClientRequestBodies() *clientRequestBodies {
	return &clientRequestBodies{byHost: map[string]string{}}
}

func (r *clientRequestBodies) filter(req *http.Request) bool {
	body := util.SafeReadBody(req)
	if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, "true") {
		return false
	}
	// Keyed off the request's own host, so a filter invoked while gock is still
	// walking another host's mock still records under the right upstream.
	r.mu.Lock()
	r.byHost[req.URL.Host] = body
	r.mu.Unlock()
	return true
}

func (r *clientRequestBodies) snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.byHost))
	for k, v := range r.byHost {
		out[k] = v
	}
	return out
}

// safeBlockForwardNetwork wires two real upstreams over mocked HTTP: rpc1
// carries the authoritative-source tag, rpc2 is an ordinary provider.
//
// The registration callback mirrors PreparedProject's — without it the state
// poller never learns the network's safeBlock config and would never poll the
// tag at all, which would make every assertion here vacuous.
func safeBlockForwardNetwork(t *testing.T, ctx context.Context, source string) *Network {
	t.Helper()

	evmUpCfg := func() *common.EvmUpstreamConfig {
		return &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(100 * time.Millisecond),
			StatePollerDebounce: common.Duration(10 * time.Millisecond),
		}
	}
	upCfgs := []*common.UpstreamConfig{
		{
			Id:       "rpc1",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Tags:     []string{safeSourceTag},
			Evm:      evmUpCfg(),
		},
		{
			Id:       "rpc2",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Tags:     []string{publicProviderTag},
			Evm:      evmUpCfg(),
		},
	}

	evmCfg := &common.EvmNetworkConfig{ChainId: 123}
	if source != "" {
		evmCfg.SafeBlock = &common.EvmSafeBlockConfig{Source: source}
	}
	nwCfg := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: evmCfg}

	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)
	mt := health.NewTracker(&log.Logger, "prjSafe", 2*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	require.NoError(t, err)

	ssCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	ssCfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, ssCfg)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(ctx, &log.Logger, "prjSafe", upCfgs, ssr, rlr, vr, pr, nil, mt,
		func(ups *upstream.Upstream) error {
			ups.SetNetworkConfig(nwCfg)
			return nil
		})
	upr.Bootstrap(ctx)
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123)))

	ntw, err := NewNetwork(ctx, &log.Logger, "prjSafe", nwCfg, rlr, upr, mt, nil)
	require.NoError(t, err)
	require.NoError(t, ntw.Bootstrap(ctx))

	return ntw
}

// mockSafeProbe answers the state poller's `safe` probe on `host` with `hex` and
// returns a counter of how many such probes that host received.
func mockSafeProbe(host, hex string) *atomic.Int64 {
	var probes atomic.Int64
	gock.New(host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"safe"`) {
				return false
			}
			// Filters run before host matching, so only count probes actually
			// aimed at this host.
			if !strings.HasSuffix(host, r.URL.Host) {
				return false
			}
			probes.Add(1)
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"number": hex, "timestamp": "0x6702a8e0"},
		})
	return &probes
}

// TestNetwork_SafeBlock_Forward_EveryUpstreamReceivesConcreteBlock is the
// headline behavior: with an authoritative source reporting a safe head, a
// `safe`-tagged eth_getBlockByNumber must arrive at whichever upstream serves it
// as the same concrete block number — never as the string "safe".
//
// eth_getBlockByNumber specifically, because its method config disables
// latest/finalized interpolation; `safe` has no such per-method escape hatch, so
// this proves the tag is resolved even on the one method that opts out of the others.
//
// rpc2 is not a source. It is wired to answer `safe` with a much higher head, so
// the test can also assert it is never asked: only the configured sources pay the
// extra poll, which is what keeps a large provider pool from taking an extra RPC
// per interval (and keeps providers that don't support the tag from being
// error-spammed). It must still receive the concrete block when it serves.
func TestNetwork_SafeBlock_Forward_EveryUpstreamReceivesConcreteBlock(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	rpc1Probes := mockSafeProbe("http://rpc1.localhost", trustedSafeHex)
	// A permissive provider running zero L1 confirmations: much higher `safe`.
	rpc2Probes := mockSafeProbe("http://rpc2.localhost", "0x9000")

	rec := newClientRequestBodies()
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).
			Post("").
			Times(1).
			Filter(rec.filter).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  map[string]interface{}{"number": trustedSafeHex, "timestamp": "0x6702a8e0"},
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw := safeBlockForwardNetwork(t, ctx, safeSourceTag)

	require.Eventually(t, func() bool {
		return ntw.EvmHighestSafeBlockNumber(ctx) == trustedSafeHead
	}, 10*time.Second, 50*time.Millisecond,
		"the authoritative source's safe head must be learned from its own poll")

	// Only the configured source is polled for the tag.
	assert.Positive(t, rpc1Probes.Load(), "the authoritative source must be polled for safe")
	assert.Zero(t, rpc2Probes.Load(), "a non-source upstream must never be polled for safe")

	// Serve the same request from each upstream in turn. The whole set is
	// re-pinned each round (the helper REPLACES the list, it does not filter),
	// so only the head of the order changes.
	for _, order := range [][]string{{"rpc1", "rpc2"}, {"rpc2", "rpc1"}} {
		servedBy := order[0]
		ntw.PinUpstreamOrderForTest(order...)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["safe",true]}`))
		resp, err := ntw.Forward(ctx, req)
		require.NoError(t, err, "safe request served by %s", servedBy)
		require.NotNil(t, resp)
		require.NotNil(t, resp.Upstream())
		assert.Equal(t, servedBy, resp.Upstream().Id())
		resp.Release()
	}

	observed := rec.snapshot()
	require.Len(t, observed, 2, "both upstreams should have served the client's request")
	for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
		body, ok := observed[host]
		require.True(t, ok, "no client request observed at %s", host)
		assert.Contains(t, body, `"`+trustedSafeHex+`"`,
			"%s must receive the trusted concrete block", host)
		assert.NotContains(t, body, "safe",
			"%s must never receive the literal safe tag", host)
	}
}

// TestNetwork_SafeBlock_Forward_UnresolvedFailsClosed pins the fail-closed rule.
//
// The source is configured but has never reported a safe head (here: it answers
// the probe with null, the shape a provider that doesn't support the tag
// produces). Forwarding `safe` verbatim would let whichever provider answers
// define it — including one running zero L1 confirmations — so the request must
// be rejected instead. The mock that would have served a verbatim `safe`
// request replies 200, so a leak shows up as a successful response, not an error.
func TestNetwork_SafeBlock_Forward_UnresolvedFailsClosed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	// The authoritative source cannot answer the tag.
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"safe"`)
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": nil})

	// Would happily serve a verbatim `safe` eth_getBalance on either upstream.
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1234"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw := safeBlockForwardNetwork(t, ctx, safeSourceTag)

	// Give the poller several intervals to try (and fail to) learn a safe head.
	time.Sleep(500 * time.Millisecond)
	require.Zero(t, ntw.EvmHighestSafeBlockNumber(ctx),
		"no authoritative safe head should be known in this fixture")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`))
	resp, err := ntw.Forward(ctx, req)
	if resp != nil {
		resp.Release()
	}

	require.Error(t, err, "an unresolvable safe request must not be forwarded")
	requireSafeBlockUnavailable(t, err, common.SafeBlockUnresolvedNoSource)
}

// TestNetwork_SafeBlock_Forward_SkipInterpolationCannotBypassTrustBoundary pins
// that a client cannot talk its way past the operator's trust boundary.
//
// `skipInterpolation` is client-supplied; `evm.safeBlock.source` is an operator
// guarantee. If the directive granted an exemption, any caller could opt itself
// back into "answer my safe request from whichever upstream replies" — the exact
// hole this feature closes. Same precedent as ErrConsensusCompositionDispute
// being non-bypassable by `disputeBehavior`: a data-trust boundary is not a
// liveness preference.
//
// The source here is HEALTHY and reporting a safe head, so this cannot pass by
// accident as an availability failure — hence the distinct reason. Both upstreams
// are wired to happily serve a verbatim `safe` eth_getBalance with 200, so a leak
// surfaces as a successful response plus a non-zero hit count.
func TestNetwork_SafeBlock_Forward_SkipInterpolationCannotBypassTrustBoundary(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	mockSafeProbe("http://rpc1.localhost", trustedSafeHex)

	var verbatimSafeHits atomic.Int64
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if !strings.Contains(body, "eth_getBalance") || !strings.Contains(body, `"safe"`) {
					return false
				}
				verbatimSafeHits.Add(1)
				return true
			}).
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1234"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw := safeBlockForwardNetwork(t, ctx, safeSourceTag)

	require.Eventually(t, func() bool {
		return ntw.EvmHighestSafeBlockNumber(ctx) == trustedSafeHead
	}, 10*time.Second, 50*time.Millisecond,
		"precondition: a source must be reporting, so a rejection can only be the directive")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`))
	// A request built straight from bytes carries NIL directives (they are
	// normally populated from HTTP headers/query), so mutating Directives()
	// in place is a no-op — the directive has to be set explicitly.
	req.SetDirectives(&common.RequestDirectives{SkipInterpolation: true})
	resp, err := ntw.Forward(ctx, req)
	if resp != nil {
		resp.Release()
	}

	require.Error(t, err, "skipInterpolation must not buy a verbatim safe forward")
	requireSafeBlockUnavailable(t, err, common.SafeBlockUnresolvedSkipInterpolation)
	assert.Zero(t, verbatimSafeHits.Load(),
		"the literal safe tag must never reach an upstream")
}

// TestNetwork_SafeBlock_Forward_UnconfiguredNetworkIsUnchanged is the
// no-regression guard: a network that never opted in must keep forwarding the
// literal tag and must never see the fail-closed rejection.
func TestNetwork_SafeBlock_Forward_UnconfiguredNetworkIsUnchanged(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var sawVerbatimSafe bool
	gock.New("http://rpc1.localhost").
		Post("").
		Times(1).
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBalance") || !strings.Contains(body, `"safe"`) {
				return false
			}
			sawVerbatimSafe = true
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1234"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ntw := safeBlockForwardNetwork(t, ctx, "")
	ntw.PinUpstreamOrderForTest("rpc1")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","safe"]}`))
	resp, err := ntw.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	resp.Release()

	assert.True(t, sawVerbatimSafe, "an opt-out network must still forward the literal safe tag")
}
