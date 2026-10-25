package evm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covPollerHandler scripts one upstream answer per JSON-RPC call.
type covPollerHandler func(method string, params []interface{}) (*common.NormalizedResponse, error)

// covPollerUpstream is a scriptable common.EvmUpstream: Forward delegates to a
// swappable handler and counts calls per method; EvmGetChainId and Cordon are
// observable so the chain-identity gate can be asserted on the poll paths.
type covPollerUpstream struct {
	cfg    *common.UpstreamConfig
	logger zerolog.Logger

	mu           sync.Mutex
	handler      covPollerHandler
	calls        map[string]int
	chainId      string
	chainErr     error
	chainIdCalls int
	cordoned     bool
	cordonReason string
}

func (u *covPollerUpstream) setHandler(h covPollerHandler) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handler = h
}

func (u *covPollerUpstream) callCount(method string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[method]
}

func (u *covPollerUpstream) isCordoned() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cordoned
}

func (u *covPollerUpstream) getChainIdCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chainIdCalls
}

func (u *covPollerUpstream) Id() string                     { return u.cfg.Id }
func (u *covPollerUpstream) VendorName() string             { return "covpoller" }
func (u *covPollerUpstream) NetworkId() string              { return "evm:123" }
func (u *covPollerUpstream) NetworkLabel() string           { return "evm:123" }
func (u *covPollerUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *covPollerUpstream) Logger() *zerolog.Logger        { return &u.logger }
func (u *covPollerUpstream) Vendor() common.Vendor          { return nil }
func (u *covPollerUpstream) Tracker() common.HealthTracker  { return nil }
func (u *covPollerUpstream) IgnoreMethod(string)            {}
func (u *covPollerUpstream) Uncordon(_, _ string)           {}
func (u *covPollerUpstream) ShouldHandleMethod(string) (bool, error) {
	return true, nil
}
func (u *covPollerUpstream) Forward(_ context.Context, nq *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.calls[jrq.Method]++
	h := u.handler
	u.mu.Unlock()
	return h(jrq.Method, jrq.Params)
}
func (u *covPollerUpstream) Cordon(_, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cordoned = true
	u.cordonReason = reason
}
func (u *covPollerUpstream) EvmGetChainId(context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chainIdCalls++
	return u.chainId, u.chainErr
}
func (u *covPollerUpstream) EvmIsBlockFinalized(context.Context, int64, bool) (bool, error) {
	return false, nil
}
func (u *covPollerUpstream) EvmAssertBlockAvailability(context.Context, string, common.AvailbilityConfidence, bool, int64) (bool, error) {
	return true, nil
}
func (u *covPollerUpstream) EvmSyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateUnknown
}
func (u *covPollerUpstream) EvmStatePoller() common.EvmStatePoller { return nil }
func (u *covPollerUpstream) EvmEffectiveLatestBlock() int64        { return 0 }
func (u *covPollerUpstream) EvmEffectiveFinalizedBlock() int64     { return 0 }
func (u *covPollerUpstream) EvmBlockAvailabilityBounds() (int64, int64) {
	return math.MinInt64, math.MaxInt64
}

var _ common.EvmUpstream = (*covPollerUpstream)(nil)

// covPollerNew builds a poller over a covPollerUpstream (chainId 123, eth_chainId
// answering "123") backed by an in-memory shared state registry. UpdateMaxWait
// is generous so the foreground TryUpdateIfStale always applies the scripted
// answer synchronously, even under -race.
func covPollerNew(t *testing.T, id string, h covPollerHandler) (*EvmStatePoller, *covPollerUpstream, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	up := &covPollerUpstream{
		cfg: &common.UpstreamConfig{
			Id:   id,
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 123},
		},
		logger:  logger,
		handler: h,
		calls:   map[string]int{},
		chainId: "123",
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
		UpdateMaxWait: common.Duration(10 * time.Second),
	})
	require.NoError(t, err)
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	return NewEvmStatePoller("test", ctx, &logger, up, tracker, ssr), up, ctx
}

// covPollerFastDebounce makes every poll past ~1ms eligible to fetch again.
func covPollerFastDebounce(p *EvmStatePoller) {
	p.stateMu.Lock()
	p.debounceInterval = time.Millisecond
	p.stateMu.Unlock()
}

func covPollerResult(result string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil))
}

func covPollerRpcError(code int, msg string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), nil,
			[]byte(fmt.Sprintf(`{"code":%d,"message":%q}`, code, msg))))
}

func covPollerBlock(num int64) *common.NormalizedResponse {
	return covPollerResult(fmt.Sprintf(`{"number":"0x%x","hash":"0x%064x","timestamp":"0x%x"}`, num, num+1, 1_700_000_000+num))
}

// covPollerBlockTag returns the first param of eth_getBlockByNumber.
func covPollerBlockTag(params []interface{}) string {
	if len(params) == 0 {
		return ""
	}
	s, _ := params[0].(string)
	return s
}

// covPollerBlocks answers latest/finalized with fixed heights and eth_syncing false.
func covPollerBlocks(latest, finalized int64) covPollerHandler {
	return func(method string, params []interface{}) (*common.NormalizedResponse, error) {
		switch method {
		case "eth_getBlockByNumber":
			switch covPollerBlockTag(params) {
			case "latest":
				return covPollerBlock(latest), nil
			case "finalized":
				return covPollerBlock(finalized), nil
			}
		case "eth_syncing":
			return covPollerResult(`false`), nil
		}
		return nil, common.NewErrEndpointUnsupported(errors.New("unexpected " + method))
	}
}

// covPollerHistory answers latest with `latest` and a numbered block with full
// data only from `earliest` upward (null below), like a pruned node.
func covPollerHistory(latest, earliest int64) covPollerHandler {
	return func(method string, params []interface{}) (*common.NormalizedResponse, error) {
		if method != "eth_getBlockByNumber" {
			return nil, common.NewErrEndpointUnsupported(errors.New("unexpected " + method))
		}
		tag := covPollerBlockTag(params)
		if tag == "latest" {
			return covPollerBlock(latest), nil
		}
		n, err := common.HexToInt64(tag)
		if err != nil {
			return nil, err
		}
		if n < earliest {
			return covPollerResult(`null`), nil
		}
		return covPollerBlock(n), nil
	}
}

func covPollerUnsupportedErrors() map[string]error {
	cause := errors.New("the method eth_getBlockByNumber does not exist/is not available")
	return map[string]error{
		"EndpointUnsupported":  common.NewErrEndpointUnsupported(cause),
		"UpstreamSkipped":      common.NewErrUpstreamRequestSkipped(cause, "u"),
		"MethodIgnored":        common.NewErrUpstreamMethodIgnored("eth_getBlockByNumber", "u"),
		"EndpointMissingData":  common.NewErrEndpointMissingData(cause, nil),
		"ClientSideException":  common.NewErrEndpointClientSideException(cause),
		"EmptyResult(no err)":  nil,
	}
}

// ---------------------------------------------------------------------------
// Latest block polling
// ---------------------------------------------------------------------------

func TestCovPoller_PollLatest_GivesUpAfterTenUnsupportedAnswers(t *testing.T) {
	for name, uerr := range covPollerUnsupportedErrors() {
		t.Run(name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-latest-unsup", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
				if uerr == nil {
					return covPollerResult(`null`), nil
				}
				return nil, uerr
			})
			covPollerFastDebounce(p)

			v, err := p.PollLatestBlockNumber(ctx)
			require.NoError(t, err, "unsupported answers are not surfaced as poll errors")
			assert.Zero(t, v)
			d := p.GetDiagnostics()
			assert.Equal(t, 1, d.LatestBlockFailureCount)
			assert.False(t, d.SkipLatestBlockCheck, "one failure must not give up yet")

			require.Eventually(t, func() bool {
				_, _ = p.PollLatestBlockNumber(ctx)
				return p.GetDiagnostics().SkipLatestBlockCheck
			}, 5*time.Second, 2*time.Millisecond)

			d = p.GetDiagnostics()
			assert.Equal(t, 10, d.LatestBlockFailureCount)
			assert.False(t, d.LatestBlockSuccessfulOnce)
			assert.Contains(t, d.LatestBlockDetectionIssue, "latest block check disabled")

			calls := up.callCount("eth_getBlockByNumber")
			v, err = p.PollLatestBlockNumber(ctx)
			require.NoError(t, err)
			assert.Zero(t, v)
			assert.Equal(t, calls, up.callCount("eth_getBlockByNumber"), "after giving up the upstream is not asked again")
		})
	}
}

func TestCovPoller_PollLatest_UnsupportedAfterSuccessNeverGivesUp(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-latest-after-success", covPollerBlocks(100, 90))
	covPollerFastDebounce(p)

	v, err := p.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(100), v)

	up.setHandler(func(string, []interface{}) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointUnsupported(errors.New("gone"))
	})
	require.Eventually(t, func() bool {
		_, _ = p.PollLatestBlockNumber(ctx)
		return up.callCount("eth_getBlockByNumber") >= 4
	}, 5*time.Second, 2*time.Millisecond)

	d := p.GetDiagnostics()
	assert.True(t, d.LatestBlockSuccessfulOnce)
	assert.Zero(t, d.LatestBlockFailureCount, "failures after a success do not count toward giving up")
	assert.False(t, d.SkipLatestBlockCheck)
	assert.Empty(t, d.LatestBlockDetectionIssue)
	assert.Equal(t, int64(100), p.LatestBlock(), "a failed poll keeps the last known head")
}

func TestCovPoller_PollLatest_TransientErrorsAreReturnedNotCounted(t *testing.T) {
	serverErr := common.NewErrEndpointServerSideException(errors.New("upstream 502"), nil, 502)
	p, _, ctx := covPollerNew(t, "cov-latest-transient", func(string, []interface{}) (*common.NormalizedResponse, error) {
		return nil, serverErr
	})

	_, err := p.PollLatestBlockNumber(ctx)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException))
	d := p.GetDiagnostics()
	assert.Zero(t, d.LatestBlockFailureCount)
	assert.False(t, d.SkipLatestBlockCheck)
	// A server-side failure is already recorded by tryForward; the poller adds nothing.
	m := p.tracker.GetUpstreamMethodMetrics(p.upstream, "eth_getBlockByNumber", common.DataFinalityStateAll)
	assert.Zero(t, m.ErrorsTotal.Load())
}

func TestCovPoller_PollLatest_OpenCircuitBreakerIsRecordedOnTracker(t *testing.T) {
	cbErr := common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, nil, nil)
	p, _, ctx := covPollerNew(t, "cov-latest-cb", func(string, []interface{}) (*common.NormalizedResponse, error) {
		return nil, cbErr
	})

	_, err := p.PollLatestBlockNumber(ctx)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))

	m := p.tracker.GetUpstreamMethodMetrics(p.upstream, "eth_getBlockByNumber", common.DataFinalityStateAll)
	assert.Equal(t, int64(1), m.RequestsTotal.Load())
	assert.Equal(t, int64(1), m.ErrorsTotal.Load(), "short-circuited polls must still move the error rate")
}

func TestCovPoller_PollLatest_MalformedBlockAnswersAreErrors(t *testing.T) {
	cases := []struct {
		name   string
		resp   *common.NormalizedResponse
		expect string
	}{
		{"json-rpc error", covPollerRpcError(-32000, "header not found"), "header not found"},
		{"block without number", covPollerResult(`{"hash":"0xabc"}`), "cannot get block number from block data"},
		{"unparseable number", covPollerResult(`{"number":"0xzz"}`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, ctx := covPollerNew(t, "cov-latest-malformed", func(string, []interface{}) (*common.NormalizedResponse, error) {
				return tc.resp, nil
			})
			v, err := p.PollLatestBlockNumber(ctx)
			require.Error(t, err)
			if tc.expect != "" {
				assert.Contains(t, err.Error(), tc.expect)
			}
			assert.Zero(t, v)
			assert.Zero(t, p.LatestBlock())
			assert.Zero(t, p.GetDiagnostics().LatestBlockFailureCount, "malformed answers are transient, not 'unsupported'")
		})
	}
}

func TestCovPoller_PollLatest_IsMonotonicAndIgnoresSmallRollbacks(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-latest-monotonic", covPollerBlocks(100, 90))
	covPollerFastDebounce(p)

	v, err := p.PollLatestBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(100), v)

	up.setHandler(covPollerBlocks(95, 90)) // lagging backend, within rollback tolerance
	calls := up.callCount("eth_getBlockByNumber")
	require.Eventually(t, func() bool {
		_, _ = p.PollLatestBlockNumber(ctx)
		return up.callCount("eth_getBlockByNumber") > calls
	}, 5*time.Second, 2*time.Millisecond)
	assert.Equal(t, int64(100), p.LatestBlock(), "small rollback must be ignored")

	p.SuggestLatestBlock(50)
	assert.Equal(t, int64(100), p.LatestBlock(), "lower suggestion must be ignored")

	up.setHandler(covPollerBlocks(120, 90))
	require.Eventually(t, func() bool {
		_, _ = p.PollLatestBlockNumber(ctx)
		return p.LatestBlock() == 120
	}, 5*time.Second, 2*time.Millisecond)
	assert.True(t, p.GetDiagnostics().LatestBlockSuccessfulOnce)
}

func TestCovPoller_PollLatest_MajorMoveChainIdGate(t *testing.T) {
	t.Run("forward jump from another chain is rejected and cordons", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-latest-gate-fwd", covPollerBlocks(5_000_000, 90))
		up.chainId = "999"
		p.latestBlockShared.TryUpdate(ctx, 1000)

		// The seed made the counter fresh: a hash need still fetches.
		_, err := covPollerFetch(p, ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1000), p.LatestBlock())
		assert.True(t, up.isCordoned())
		assert.Contains(t, up.cordonReason, "chain identity mismatch on major latest head move")
	})

	t.Run("large rollback from another chain is rejected and cordons", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-latest-gate-back", covPollerBlocks(1000, 90))
		up.chainId = "999"
		p.latestBlockShared.TryUpdate(ctx, 5_000_000)

		_, err := covPollerFetch(p, ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(5_000_000), p.LatestBlock())
		assert.True(t, up.isCordoned(), "a major backward move is gated just like a forward one")
	})

	t.Run("verified major jump is accepted", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-latest-gate-ok", covPollerBlocks(5_000_000, 90))
		p.latestBlockShared.TryUpdate(ctx, 1000)

		v, err := covPollerFetch(p, ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(5_000_000), v)
		assert.Equal(t, 1, up.getChainIdCalls())
		assert.False(t, up.isCordoned())
	})

	t.Run("upstream without EvmGetChainId cannot be verified and passes", func(t *testing.T) {
		lg := zerolog.Nop()
		cu := &canaryUpstream{
			logger: lg,
			cfg: &common.UpstreamConfig{
				Id: "cov-latest-gate-plain", Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{ChainId: 123},
			},
			handler: func(string, string) (*common.NormalizedResponse, error) { return nil, nil },
		}
		base, _, _ := covPollerNew(t, "cov-latest-gate-plain-base", covPollerBlocks(1, 1))
		p := NewEvmStatePoller("test", context.Background(), &lg, cu, base.tracker, base.sharedStateRegistry)
		assert.True(t, p.verifyChainIdOnMajorHeadMove(context.Background(), "latest", 1000, 5_000_000))
	})
}

// covPollerFetch is an Ask with a hash need, which fetches however fresh
// the counter is: the counter after it and its error.
func covPollerFetch(p *EvmStatePoller, ctx context.Context) (int64, error) {
	a := p.Ask(ctx, common.HeadNeedHash, common.AskRefresh)
	return a.Block, a.Err
}

// ---------------------------------------------------------------------------
// Finalized block polling
// ---------------------------------------------------------------------------

func TestCovPoller_PollFinalized_GivesUpAfterTenUnsupportedAnswers(t *testing.T) {
	for _, name := range []string{"EmptyResult(no err)", "ClientSideException", "EndpointUnsupported"} {
		uerr := covPollerUnsupportedErrors()[name]
		t.Run(name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-fin-unsup", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
				if uerr == nil {
					return covPollerResult(`null`), nil
				}
				return nil, uerr
			})
			covPollerFastDebounce(p)

			v, err := p.PollFinalizedBlockNumber(ctx)
			require.NoError(t, err)
			assert.Zero(t, v)
			assert.Equal(t, 1, p.GetDiagnostics().FinalizedBlockFailureCount)

			require.Eventually(t, func() bool {
				_, _ = p.PollFinalizedBlockNumber(ctx)
				return p.GetDiagnostics().SkipFinalizedCheck
			}, 5*time.Second, 2*time.Millisecond)

			d := p.GetDiagnostics()
			assert.Equal(t, 10, d.FinalizedBlockFailureCount)
			assert.Contains(t, d.FinalizedBlockDetectionIssue, "finalized block check disabled")

			calls := up.callCount("eth_getBlockByNumber")
			v, err = p.PollFinalizedBlockNumber(ctx)
			require.NoError(t, err)
			assert.Zero(t, v)
			assert.Equal(t, calls, up.callCount("eth_getBlockByNumber"))
		})
	}
}

func TestCovPoller_PollFinalized_UnsupportedAfterSuccessNeverGivesUp(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-fin-after-success", covPollerBlocks(100, 90))
	covPollerFastDebounce(p)

	v, err := p.PollFinalizedBlockNumber(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(90), v)

	up.setHandler(func(string, []interface{}) (*common.NormalizedResponse, error) {
		return covPollerResult(`null`), nil
	})
	require.Eventually(t, func() bool {
		_, _ = p.PollFinalizedBlockNumber(ctx)
		return up.callCount("eth_getBlockByNumber") >= 4
	}, 5*time.Second, 2*time.Millisecond)

	d := p.GetDiagnostics()
	assert.True(t, d.FinalizedBlockSuccessfulOnce)
	assert.Zero(t, d.FinalizedBlockFailureCount)
	assert.False(t, d.SkipFinalizedCheck)
	assert.Equal(t, int64(90), p.FinalizedBlock())
}

func TestCovPoller_PollFinalized_OpenCircuitBreakerIsRecordedOnTracker(t *testing.T) {
	cbErr := common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, nil, nil)
	p, _, ctx := covPollerNew(t, "cov-fin-cb", func(string, []interface{}) (*common.NormalizedResponse, error) {
		return nil, cbErr
	})

	_, err := p.PollFinalizedBlockNumber(ctx)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))
	m := p.tracker.GetUpstreamMethodMetrics(p.upstream, "eth_getBlockByNumber", common.DataFinalityStateAll)
	assert.Equal(t, int64(1), m.RequestsTotal.Load())
	assert.Equal(t, int64(1), m.ErrorsTotal.Load())
	assert.Zero(t, p.GetDiagnostics().FinalizedBlockFailureCount)
}

func TestCovPoller_PollFinalized_MajorJumpFromAnotherChainIsRejected(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-fin-gate", covPollerBlocks(5_000_100, 5_000_000))
	up.chainId = "999"
	covPollerFastDebounce(p)
	p.finalizedBlockShared.TryUpdate(ctx, 1000) // marks the shared value fresh

	require.Eventually(t, func() bool {
		v, err := p.PollFinalizedBlockNumber(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1000), v)
		return up.isCordoned()
	}, 5*time.Second, 2*time.Millisecond)
	assert.Equal(t, int64(1000), p.FinalizedBlock())
	assert.Contains(t, up.cordonReason, "major finalized head move")
}

// ---------------------------------------------------------------------------
// Debounce resolution
// ---------------------------------------------------------------------------

func TestCovPoller_ResolveDebounce_DynamicMultiplierScalesBlockTime(t *testing.T) {
	p, up, _ := covPollerNew(t, "cov-debounce-mult", covPollerBlocks(1, 1))
	feedBlockTime(t, p, up, 1, 12) // 12s blocks

	half := 0.5
	bt := p.tracker.GetNetworkBlockTime(up.NetworkId())
	assert.Equal(t, time.Duration(float64(bt)*half), p.resolveDebounce(&common.EvmNetworkConfig{DynamicBlockTimeDebounceMultiplier: &half}))

	zero := 0.0
	assert.Equal(t, time.Duration(float64(bt)*common.DefaultDynamicBlockTimeDebounceMultiplier),
		p.resolveDebounce(&common.EvmNetworkConfig{DynamicBlockTimeDebounceMultiplier: &zero}),
		"a non-positive multiplier falls back to the default")
}

func TestCovPoller_ResolveFinalizedDebounce(t *testing.T) {
	const base = 2 * time.Second
	half := 0.5
	zero := 0.0

	newPoller := func(t *testing.T) (*EvmStatePoller, *covPollerUpstream, context.Context) {
		p, up, ctx := covPollerNew(t, "cov-fin-debounce", covPollerBlocks(1, 1))
		feedBlockTime(t, p, up, 1, 12)
		p.finalizedBlockShared.TryUpdate(ctx, 1_000_000)
		p.latestBlockShared.TryUpdate(ctx, 1_000_100)
		return p, up, ctx
	}

	t.Run("widened to a fraction of the finality window", func(t *testing.T) {
		p, _, _ := newPoller(t)
		got := p.resolveFinalizedDebounce(&common.EvmNetworkConfig{FinalizedDebounceDepthMultiplier: &half}, base)
		bt := p.tracker.GetNetworkBlockTime(p.upstream.NetworkId())
		require.Greater(t, time.Duration(float64(100)*float64(bt)*half), base)
		assert.Equal(t, time.Duration(float64(100)*float64(bt)*half), got)
	})

	t.Run("never narrower than the regular debounce", func(t *testing.T) {
		p, _, _ := newPoller(t)
		got := p.resolveFinalizedDebounce(&common.EvmNetworkConfig{FinalizedDebounceDepthMultiplier: &half}, time.Hour)
		assert.Equal(t, time.Hour, got)
	})

	t.Run("non-positive multiplier keeps the regular debounce", func(t *testing.T) {
		p, _, _ := newPoller(t)
		assert.Equal(t, base, p.resolveFinalizedDebounce(&common.EvmNetworkConfig{FinalizedDebounceDepthMultiplier: &zero}, base))
	})

	t.Run("unset multiplier keeps the regular debounce", func(t *testing.T) {
		p, _, _ := newPoller(t)
		assert.Equal(t, base, p.resolveFinalizedDebounce(&common.EvmNetworkConfig{}, base))
		assert.Equal(t, base, p.resolveFinalizedDebounce(nil, base))
	})

	t.Run("upstream behind the network finalized keeps the regular debounce", func(t *testing.T) {
		p, up, _ := newPoller(t)
		// The tracker learns of a higher finalized block than this poller holds.
		p.tracker.SetFinalizedBlockNumber(up, 1_000_050)
		require.Equal(t, int64(1_000_050), p.tracker.GetNetworkFinalizedBlock(up.NetworkId()))
		assert.Equal(t, base, p.resolveFinalizedDebounce(&common.EvmNetworkConfig{FinalizedDebounceDepthMultiplier: &half}, base))
	})
}

// ---------------------------------------------------------------------------
// Suggest / refresh / callbacks
// ---------------------------------------------------------------------------

func TestCovPoller_SuggestLatestBlock_MajorJumpWhileVerificationInFlightIsDropped(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-suggest-inflight", covPollerBlocks(1, 1))
	p.latestBlockShared.TryUpdate(ctx, 1000)

	p.latestMajorVerifyInProgress.Lock()
	p.SuggestLatestBlock(5_000_000)
	// The busy path returns synchronously without spawning any verification.
	assert.Equal(t, int64(1000), p.LatestBlock())
	assert.Zero(t, up.getChainIdCalls())
	p.latestMajorVerifyInProgress.Unlock()

	p.SuggestLatestBlock(5_000_000)
	require.Eventually(t, func() bool { return p.LatestBlock() == 5_000_000 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, up.getChainIdCalls())
}

func TestCovPoller_VerifyThenSuggest_StaleSuggestionIsDiscarded(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-suggest-stale", covPollerBlocks(1, 1))
	p.latestBlockShared.TryUpdate(ctx, 1000)

	// Queued major suggestion that another path already overtook.
	p.verifyThenSuggestLatestBlock(500)
	require.Eventually(t, func() bool {
		if p.latestMajorVerifyInProgress.TryLock() {
			p.latestMajorVerifyInProgress.Unlock()
			return true
		}
		return false
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(1000), p.LatestBlock())
	assert.Zero(t, up.getChainIdCalls(), "an overtaken suggestion is not verified")
}

func TestCovPoller_SuggestFinalizedBlock_SkippedWhileUpdateInProgress(t *testing.T) {
	p, _, _ := covPollerNew(t, "cov-suggest-fin-busy", covPollerBlocks(1, 1))

	p.finalizedUpdateInProgress.Lock()
	p.SuggestFinalizedBlock(500)
	assert.Zero(t, p.FinalizedBlock(), "a concurrent suggestion is dropped synchronously")
	p.finalizedUpdateInProgress.Unlock()

	p.SuggestFinalizedBlock(500)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == 500 }, 5*time.Second, 5*time.Millisecond)

	p.SuggestFinalizedBlock(400)
	require.Eventually(t, func() bool {
		if p.finalizedUpdateInProgress.TryLock() {
			p.finalizedUpdateInProgress.Unlock()
			return true
		}
		return false
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, int64(500), p.FinalizedBlock(), "lower finalized suggestions never move the head back")
}

func TestCovPoller_OnLatestBlock_FiresForEveryAdvance(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-onlatest", covPollerBlocks(300, 1))
	got := make(chan int64, 8)
	p.OnLatestBlock(func(v int64) { got <- v })

	p.SuggestLatestBlock(200)
	assert.Equal(t, int64(200), <-got)

	// The suggestion made the counter fresh, which meets a number need;
	// a hash need fetches.
	require.NoError(t, p.Ask(ctx, common.HeadNeedHash, common.AskRefresh).Err)
	assert.Equal(t, int64(300), <-got, "polled advances fire the same callback")
	assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"))

	p.SuggestLatestBlock(250)
	select {
	case v := <-got:
		t.Fatalf("callback fired for a non-advance: %d", v)
	default:
	}
}

// covPollerDebounce sets the poller's cadence to d.
func covPollerDebounce(p *EvmStatePoller, d time.Duration) {
	p.stateMu.Lock()
	p.debounceInterval = d
	p.stateMu.Unlock()
}

// TestCovPoller_Ask holds the Ask's contract (spec §6.8): one fetch per
// upstream in flight, its outcome to every waiter; only an answer sent
// within the cadence debounces; node failures back off; a panic fails
// every waiter.
func TestCovPoller_Ask(t *testing.T) {
	t.Run("unusable pollers fetch nothing", func(t *testing.T) {
		var nilPoller *EvmStatePoller
		assert.True(t, nilPoller.IsObjectNull())
		assert.Equal(t, common.AskBackoff, nilPoller.Ask(context.Background(), common.HeadNeedHash, common.AskRefresh).Outcome)
		assert.Nil(t, nilPoller.GetDiagnostics())

		up := &covPollerUpstream{cfg: &common.UpstreamConfig{Id: "x"}, calls: map[string]int{}}
		bare := &EvmStatePoller{upstream: up}
		assert.False(t, bare.IsObjectNull())
		bare.AskBackground(common.HeadNeedNumber, common.AskRefresh)
		bare.OnLatestBlock(func(int64) {})
		assert.Zero(t, up.callCount("eth_getBlockByNumber"), "no shared counter, nothing to refresh")
	})

	t.Run("every waiter gets the one fetch's answer", func(t *testing.T) {
		release := make(chan struct{})
		p, up, ctx := covPollerNew(t, "cov-ask-join", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
			<-release
			return covPollerBlocks(77, 1)(method, params)
		})
		// A cadence far shorter than the fetch: a fetch in flight is
		// joined however old it is.
		covPollerDebounce(p, time.Millisecond)
		const waiters = 20
		answers := make(chan common.HeadAnswer, waiters)
		for range waiters {
			go func() { answers <- p.Ask(ctx, common.HeadNeedHash, common.AskRefresh) }()
		}
		require.Eventually(t, func() bool { return up.callCount("eth_getBlockByNumber") == 1 }, 5*time.Second, time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		close(release)
		outcomes := map[common.AskOutcome]int{}
		for range waiters {
			a := <-answers
			require.NoError(t, a.Err)
			assert.Equal(t, int64(77), a.Block)
			assert.Contains(t, string(a.Header), `"number":"0x4d"`)
			outcomes[a.Outcome]++
		}
		assert.Equal(t, map[common.AskOutcome]int{common.AskFetched: 1, common.AskInflight: waiters - 1}, outcomes)
		assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"), "one fetch for every waiter")
	})

	t.Run("an answer within the cadence debounces; the counter meets a number need", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-ask-debounce", covPollerBlocks(42, 1))
		covPollerDebounce(p, time.Hour)
		first := p.Ask(ctx, common.HeadNeedHash, common.AskRefresh)
		require.NoError(t, first.Err)
		assert.Equal(t, common.AskFetched, first.Outcome)
		again := p.Ask(ctx, common.HeadNeedHash, common.AskRefresh)
		assert.Equal(t, common.AskInflight, again.Outcome)
		assert.Equal(t, first.Header, again.Header)
		assert.Equal(t, common.AskSharedFresh, p.Ask(ctx, common.HeadNeedNumber, common.AskRefresh).Outcome)
		assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"))
	})

	t.Run("node failures back off; the backoff ends with the next answer", func(t *testing.T) {
		var failing sync.Mutex
		fail := true
		p, up, ctx := covPollerNew(t, "cov-ask-backoff", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
			failing.Lock()
			defer failing.Unlock()
			if fail {
				return nil, common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)
			}
			return covPollerBlocks(9, 1)(method, params)
		})
		const cadence = 50 * time.Millisecond
		covPollerDebounce(p, cadence)
		a := p.Ask(ctx, common.HeadNeedNumber, common.AskRefresh)
		require.Error(t, a.Err)
		assert.Equal(t, common.AskFetched, a.Outcome)
		assert.Equal(t, 2*cadence, p.BackedOffCadence(), "the bench reads the backoff")
		b := p.Ask(ctx, common.HeadNeedNumber, common.AskRefresh)
		assert.Equal(t, common.AskBackoff, b.Outcome)
		assert.NoError(t, b.Err, "a backoff fetches nothing and fails nothing")
		assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"))
		time.Sleep(2 * cadence)
		failing.Lock()
		fail = false
		failing.Unlock()
		c := p.Ask(ctx, common.HeadNeedNumber, common.AskRefresh)
		require.NoError(t, c.Err)
		assert.Equal(t, common.AskFetched, c.Outcome)
		assert.Equal(t, int64(9), c.Block)
		assert.Equal(t, cadence, p.backoff())
	})

	t.Run("a panic fails every waiter and never debounces", func(t *testing.T) {
		release := make(chan struct{})
		var panicking sync.Mutex
		explode := true
		p, up, ctx := covPollerNew(t, "cov-ask-panic", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
			panicking.Lock()
			boom := explode
			panicking.Unlock()
			if boom {
				<-release
				panic("vendor client bug")
			}
			return covPollerBlocks(5, 1)(method, params)
		})
		covPollerDebounce(p, time.Hour)
		panics := telemetry.MetricUnexpectedPanicTotal.WithLabelValues("head-ask",
			"project:test network:n/a upstream:cov-ask-panic where:fetch", common.ErrorFingerprint("vendor client bug"))
		before := promUtil.ToFloat64(panics)
		answers := make(chan common.HeadAnswer, 3)
		for range 3 {
			go func() { answers <- p.Ask(ctx, common.HeadNeedHash, common.AskRefresh) }()
		}
		require.Eventually(t, func() bool { return up.callCount("eth_getBlockByNumber") == 1 }, 5*time.Second, time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		close(release)
		for range 3 {
			a := <-answers
			require.ErrorContains(t, a.Err, "panicked")
			assert.Nil(t, a.Header)
		}
		assert.Equal(t, before+1, promUtil.ToFloat64(panics))
		panicking.Lock()
		explode = false
		panicking.Unlock()
		a := p.Ask(ctx, common.HeadNeedHash, common.AskRefresh)
		require.NoError(t, a.Err)
		assert.Equal(t, common.AskFetched, a.Outcome, "a failed fetch never debounces")
		assert.Equal(t, int64(5), a.Block)
	})
}

// ---------------------------------------------------------------------------
// Shared state integration, config, diagnostics
// ---------------------------------------------------------------------------

func TestCovPoller_LargeRollbackIsReportedToTracker(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-rollback-gauge", covPollerBlocks(1, 1))

	p.latestBlockShared.TryUpdate(ctx, 10_000)
	p.latestBlockShared.TryUpdate(ctx, 3_000) // beyond tolerance: a real reorg/correction
	p.finalizedBlockShared.TryUpdate(ctx, 9_000)
	p.finalizedBlockShared.TryUpdate(ctx, 2_000)

	assert.Equal(t, int64(3_000), p.LatestBlock())
	assert.Equal(t, int64(2_000), p.FinalizedBlock())
	latestGauge := telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues("test", up.VendorName(), up.NetworkLabel(), up.Id(), "latest")
	finalizedGauge := telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues("test", up.VendorName(), up.NetworkLabel(), up.Id(), "finalized")
	assert.Equal(t, float64(7_000), promUtil.ToFloat64(latestGauge))
	assert.Equal(t, float64(7_000), promUtil.ToFloat64(finalizedGauge))
}

func TestCovPoller_SetNetworkConfig(t *testing.T) {
	p, _, _ := covPollerNew(t, "cov-netcfg", covPollerBlocks(1, 1))

	p.SetNetworkConfig(nil)
	p.SetNetworkConfig(&common.NetworkConfig{Architecture: common.ArchitectureEvm})
	assert.Nil(t, p.cfg, "nil configs are ignored")
	assert.Equal(t, "n/a", p.networkLabel)

	p.SetNetworkConfig(&common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}})
	assert.Equal(t, "evm:123", p.networkLabel)

	p.SetNetworkConfig(&common.NetworkConfig{Architecture: common.ArchitectureEvm, Alias: "mainnet", Evm: &common.EvmNetworkConfig{ChainId: 123}})
	assert.Equal(t, "mainnet", p.networkLabel, "alias wins over the network id")
}

func TestCovPoller_GetDiagnostics_OperatorSkipAndEarliest(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-diag", covPollerHistory(100, 42))
	skip := true
	up.cfg.Evm.SkipSyncingCheck = &skip

	_, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Hour)
	require.NoError(t, err)

	d := p.GetDiagnostics()
	assert.True(t, d.SkipSyncingCheck, "operator-configured skip is reflected")
	assert.NotEmpty(t, d.SyncingCheckError)
	require.Contains(t, d.EarliestByProbe, common.EvmProbeBlockHeader)
	info := d.EarliestByProbe[common.EvmProbeBlockHeader]
	assert.Equal(t, int64(42), info.EarliestBlock)
	assert.False(t, info.SchedulerRunning)
	assert.Equal(t, int64(100), d.LatestBlock)
}

// ---------------------------------------------------------------------------
// Syncing detection
// ---------------------------------------------------------------------------

func TestCovPoller_FetchSyncingState(t *testing.T) {
	cases := []struct {
		name    string
		resp    *common.NormalizedResponse
		err     error
		syncing bool
		wantErr string
		skip    bool
	}{
		{name: "bool true", resp: covPollerResult(`true`), syncing: true},
		{name: "bool false", resp: covPollerResult(`false`)},
		{name: "geth progress object", resp: covPollerResult(`{"currentBlock":"0x10","highestBlock":"0x20"}`), syncing: true},
		{name: "arbitrum msgCount", resp: covPollerResult(`{"msgCount":12}`), syncing: true},
		{name: "Ok true", resp: covPollerResult(`{"Ok":true}`)},
		{name: "Ok false", resp: covPollerResult(`{"Ok":false}`), syncing: true},
		{name: "ok true", resp: covPollerResult(`{"ok":true}`)},
		{name: "ok false", resp: covPollerResult(`{"ok":false}`), syncing: true},
		{name: "unknown object", resp: covPollerResult(`{"foo":1}`), wantErr: "cannot parse syncing state"},
		{name: "string result", resp: covPollerResult(`"yes"`), wantErr: "cannot parse syncing state"},
		{name: "not json", resp: covPollerResult(`nope`), wantErr: "cannot parse syncing state"},
		{name: "json-rpc error", resp: covPollerRpcError(-32601, "the method eth_syncing does not exist/is not available"), wantErr: "does not exist/is not available"},
		{name: "unsupported marks skip", err: common.NewErrEndpointUnsupported(errors.New("no")), wantErr: "does not support", skip: true},
		{name: "client error marks skip", err: common.NewErrEndpointClientSideException(errors.New("bad")), wantErr: "client-side", skip: true},
		{name: "server error keeps checking", err: common.NewErrEndpointServerSideException(errors.New("down"), nil, 503), wantErr: "internal error on remote endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, ctx := covPollerNew(t, "cov-syncing", func(string, []interface{}) (*common.NormalizedResponse, error) {
				return tc.resp, tc.err
			})
			syncing, err := p.fetchSyncingState(ctx)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.syncing, syncing)
			}
			assert.Equal(t, tc.skip, p.GetDiagnostics().SkipSyncingCheck)
		})
	}
}

func TestCovPoller_Poll_SyncingStateTransitions(t *testing.T) {
	var mu sync.Mutex
	syncing := true
	h := func(method string, params []interface{}) (*common.NormalizedResponse, error) {
		if method == "eth_syncing" {
			mu.Lock()
			defer mu.Unlock()
			return covPollerResult(fmt.Sprintf("%t", syncing)), nil
		}
		return covPollerBlocks(100, 90)(method, params)
	}
	setSyncing := func(v bool) {
		mu.Lock()
		syncing = v
		mu.Unlock()
	}
	p, up, ctx := covPollerNew(t, "cov-syncing-poll", h)

	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, common.EvmSyncingStateSyncing, p.SyncingState())
	assert.Equal(t, int64(100), p.LatestBlock())
	assert.Equal(t, int64(90), p.FinalizedBlock())

	setSyncing(false)
	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState(), "one 'synced' answer is not yet trusted")

	setSyncing(true)
	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, common.EvmSyncingStateSyncing, p.SyncingState(), "a syncing answer resets confirmations")

	setSyncing(false)
	for range FullySyncedThreshold - 2 {
		require.NoError(t, p.Poll(ctx))
		assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())
	}
	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, common.EvmSyncingStateNotSyncing, p.SyncingState())

	calls := up.callCount("eth_syncing")
	require.NoError(t, p.Poll(ctx))
	assert.Equal(t, calls, up.callCount("eth_syncing"), "fully synced nodes are not asked again")

	p.SetSyncingState(common.EvmSyncingStateSyncing)
	assert.Equal(t, common.EvmSyncingStateSyncing, p.SyncingState())
}

func TestCovPoller_Poll_JoinsErrorsFromEachTracker(t *testing.T) {
	p, _, ctx := covPollerNew(t, "cov-poll-errs", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
		switch method {
		case "eth_getBlockByNumber":
			if covPollerBlockTag(params) == "latest" {
				return nil, common.NewErrEndpointServerSideException(errors.New("latest down"), nil, 502)
			}
			return covPollerBlock(90), nil
		case "eth_syncing":
			return nil, common.NewErrEndpointServerSideException(errors.New("syncing down"), nil, 502)
		}
		return nil, errors.New("unexpected")
	})

	err := p.Poll(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "latest down")
	assert.Contains(t, err.Error(), "syncing down")
	assert.Equal(t, int64(90), p.FinalizedBlock(), "a healthy tracker still updates when others fail")
	assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())
}

// ---------------------------------------------------------------------------
// Earliest block detection
// ---------------------------------------------------------------------------

func TestCovPoller_PollEarliestBlockNumber(t *testing.T) {
	t.Run("bootstraps latest then binary-searches the earliest block", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-earliest-ok", covPollerHistory(100, 42))
		assert.Zero(t, p.EarliestBlock(common.EvmProbeBlockHeader), "unknown probe reads as 0")

		v, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
		assert.Equal(t, int64(42), p.EarliestBlock(common.EvmProbeBlockHeader))
		assert.Equal(t, int64(100), p.LatestBlock())

		calls := up.callCount("eth_getBlockByNumber")
		v, err = p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(42), v)
		assert.Equal(t, calls, up.callCount("eth_getBlockByNumber"), "fresh earliest is served without probing")
	})

	t.Run("latest unavailable fails the detection", func(t *testing.T) {
		p, _, ctx := covPollerNew(t, "cov-earliest-nolatest", func(string, []interface{}) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointServerSideException(errors.New("latest down"), nil, 502)
		})
		_, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "latest down")
		assert.Zero(t, p.EarliestBlock(common.EvmProbeBlockHeader))
	})

	t.Run("latest empty yields no earliest bound", func(t *testing.T) {
		p, _, ctx := covPollerNew(t, "cov-earliest-emptylatest", func(string, []interface{}) (*common.NormalizedResponse, error) {
			return covPollerResult(`null`), nil
		})
		v, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Hour)
		require.NoError(t, err)
		assert.Zero(t, v)
	})
}

func TestCovPoller_InitializeEarliestDetection(t *testing.T) {
	plus := int64(0)
	availability := func() *common.EvmBlockAvailabilityConfig {
		return &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: &plus, UpdateRate: common.Duration(time.Hour)},
			Upper: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: &plus, UpdateRate: common.Duration(30 * time.Minute)},
		}
	}

	t.Run("latest poll error skips this cycle", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-init-err", func(string, []interface{}) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointServerSideException(errors.New("down"), nil, 502)
		})
		up.cfg.Evm.BlockAvailability = availability()
		p.initializeEarliestBlockDetectionAndStartScheduler(ctx)
		assert.Nil(t, p.GetDiagnostics().EarliestByProbe)
		assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"), "only the latest poll was attempted")
	})

	t.Run("latest still empty skips this cycle", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-init-empty", func(string, []interface{}) (*common.NormalizedResponse, error) {
			return covPollerResult(`null`), nil
		})
		up.cfg.Evm.BlockAvailability = availability()
		p.initializeEarliestBlockDetectionAndStartScheduler(ctx)
		assert.Nil(t, p.GetDiagnostics().EarliestByProbe)
	})

	t.Run("detects once and starts one scheduler per probe", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-init-ok", covPollerHistory(100, 42))
		up.cfg.Evm.BlockAvailability = availability()

		p.initializeEarliestBlockDetectionAndStartScheduler(ctx)
		d := p.GetDiagnostics()
		require.Contains(t, d.EarliestByProbe, common.EvmProbeBlockHeader, "an empty probe defaults to blockHeader")
		assert.Equal(t, int64(42), d.EarliestByProbe[common.EvmProbeBlockHeader].EarliestBlock)
		assert.True(t, d.EarliestByProbe[common.EvmProbeBlockHeader].SchedulerRunning)

		calls := up.callCount("eth_getBlockByNumber")
		p.initializeEarliestBlockDetectionAndStartScheduler(ctx)
		assert.Equal(t, calls, up.callCount("eth_getBlockByNumber"), "initial detection runs once per instance")
	})
}

// ---------------------------------------------------------------------------
// Availability probes
// ---------------------------------------------------------------------------

func TestCovPoller_BinarySearchEarliest_Bounds(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-bsearch", covPollerHistory(1000, 321))

	v, err := p.binarySearchEarliest(ctx, "", -10, 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(321), v, "empty probe defaults to blockHeader and negative low is clamped")

	calls := up.callCount("eth_getBlockByNumber")
	v, err = p.binarySearchEarliest(ctx, common.EvmProbeBlockHeader, 0, -1)
	require.NoError(t, err)
	assert.Zero(t, v)
	assert.Equal(t, calls, up.callCount("eth_getBlockByNumber"), "an empty range probes nothing")

	p2, _, _ := covPollerNew(t, "cov-bsearch-genesis", covPollerHistory(1000, 1))
	v, err = p2.binarySearchEarliest(ctx, common.EvmProbeBlockHeader, 0, 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(1), v, "block-1 fast path")
}

func TestCovPoller_CheckProbe_DispatchAndUnknown(t *testing.T) {
	p, up, ctx := covPollerNew(t, "cov-probe-dispatch", covPollerHistory(1000, 0))

	ok, unsupported, err := p.checkProbe(ctx, "", 5)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, unsupported)

	before := up.callCount("eth_getBlockByNumber")
	ok, unsupported, err = p.checkProbe(ctx, "bogusProbe", 5)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.True(t, unsupported, "unknown probes report unsupported")
	assert.Equal(t, before, up.callCount("eth_getBlockByNumber"))
}

func TestCovPoller_CheckBlockHeaderProbe(t *testing.T) {
	cases := []struct {
		name  string
		block int64
		resp  *common.NormalizedResponse
		err   error
		want  bool
	}{
		{name: "negative block", block: -1, want: false},
		{name: "rpc error", block: 5, resp: covPollerRpcError(-32000, "missing trie node")},
		{name: "null", block: 5, resp: covPollerResult(`null`)},
		{name: "transport error", block: 5, err: errors.New("dial tcp: connection refused")},
		{name: "available", block: 5, resp: covPollerBlock(5), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-probe-header", func(string, []interface{}) (*common.NormalizedResponse, error) {
				return tc.resp, tc.err
			})
			ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeBlockHeader, tc.block)
			require.NoError(t, err)
			assert.False(t, unsupported)
			assert.Equal(t, tc.want, ok)
			if tc.block < 0 {
				assert.Zero(t, up.callCount("eth_getBlockByNumber"))
			}
		})
	}
}

func TestCovPoller_CheckEventLogsProbe(t *testing.T) {
	type logsResp struct {
		resp *common.NormalizedResponse
		err  error
	}
	cases := []struct {
		name        string
		block       int64
		header      *common.NormalizedResponse
		logs        *logsResp
		ok          bool
		unsupported bool
	}{
		{name: "negative block", block: -1},
		{name: "block missing", block: 5, header: covPollerResult(`null`)},
		{name: "block header rpc error", block: 5, header: covPollerRpcError(-32000, "missing trie node")},
		{name: "block without hash", block: 5, header: covPollerResult(`{"number":"0x5"}`)},
		{name: "one log", block: 5, header: covPollerBlock(5), logs: &logsResp{resp: covPollerResult(`[{"logIndex":"0x0"}]`)}, ok: true},
		{name: "no logs", block: 5, header: covPollerBlock(5), logs: &logsResp{resp: covPollerResult(`[]`)}},
		{name: "non-array result", block: 5, header: covPollerBlock(5), logs: &logsResp{resp: covPollerResult(`{"a":1}`)}},
		{name: "method not found", block: 5, header: covPollerBlock(5), logs: &logsResp{resp: covPollerRpcError(-32601, "the method eth_getLogs does not exist/is not available")}, unsupported: true},
		{name: "other rpc error", block: 5, header: covPollerBlock(5), logs: &logsResp{resp: covPollerRpcError(-32005, "query returned more than 10000 results")}},
		{name: "unsupported error", block: 5, header: covPollerBlock(5), logs: &logsResp{err: common.NewErrEndpointUnsupported(errors.New("no logs"))}, unsupported: true},
		{name: "ignored method", block: 5, header: covPollerBlock(5), logs: &logsResp{err: common.NewErrUpstreamMethodIgnored("eth_getLogs", "u")}, unsupported: true},
		{name: "transport error", block: 5, header: covPollerBlock(5), logs: &logsResp{err: errors.New("i/o timeout")}},
		{name: "empty response", block: 5, header: covPollerBlock(5), logs: &logsResp{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-probe-logs", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
				switch method {
				case "eth_getBlockByNumber":
					return tc.header, nil
				case "eth_getLogs":
					require.NotNil(t, tc.logs, "eth_getLogs must not be called")
					if f, ok := params[0].(map[string]interface{}); ok {
						assert.True(t, strings.HasPrefix(fmt.Sprint(f["blockHash"]), "0x"))
					}
					return tc.logs.resp, tc.logs.err
				}
				return nil, errors.New("unexpected")
			})
			ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeEventLogs, tc.block)
			require.NoError(t, err)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.unsupported, unsupported)
			if tc.logs == nil {
				assert.Zero(t, up.callCount("eth_getLogs"))
			}
		})
	}
}

func TestCovPoller_CheckCallStateProbe_FallbackPaths(t *testing.T) {
	cases := []struct {
		name        string
		callErr     error
		balResp     *common.NormalizedResponse
		balErr      error
		ok          bool
		unsupported bool
		balanceUsed bool
	}{
		{name: "eth_call unsupported", callErr: common.NewErrEndpointUnsupported(errors.New("no")), unsupported: true},
		{name: "balance unsupported", callErr: errors.New("execution reverted"), balErr: common.NewErrUpstreamRequestSkipped(errors.New("x"), "u"), unsupported: true, balanceUsed: true},
		{name: "balance transport error", callErr: errors.New("execution reverted"), balErr: errors.New("i/o timeout"), balanceUsed: true},
		{name: "balance rpc error", callErr: errors.New("execution reverted"), balResp: covPollerRpcError(-32000, "missing trie node"), balanceUsed: true},
		{name: "balance null", callErr: errors.New("execution reverted"), balResp: covPollerResult(`null`), balanceUsed: true},
		{name: "balance empty response", callErr: errors.New("execution reverted"), balanceUsed: true},
		{name: "balance answered", callErr: errors.New("execution reverted"), balResp: covPollerResult(`"0x0"`), ok: true, balanceUsed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-probe-callstate", func(method string, params []interface{}) (*common.NormalizedResponse, error) {
				switch method {
				case "eth_call":
					return nil, tc.callErr
				case "eth_getBalance":
					return tc.balResp, tc.balErr
				}
				return nil, errors.New("unexpected")
			})
			ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeCallState, 0x99)
			require.NoError(t, err)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.unsupported, unsupported)
			assert.Equal(t, tc.balanceUsed, up.callCount("eth_getBalance") == 1)
		})
	}

	t.Run("negative block", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-probe-callstate-neg", covPollerBlocks(1, 1))
		ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeCallState, -3)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Zero(t, up.callCount("eth_call"))
	})
}

func TestCovPoller_CheckTraceDataProbe(t *testing.T) {
	notFound := func(m string) *common.NormalizedResponse {
		return covPollerRpcError(-32601, "the method "+m+" does not exist/is not available")
	}
	type ans struct {
		resp *common.NormalizedResponse
		err  error
	}
	cases := []struct {
		name        string
		header      *common.NormalizedResponse
		answers     map[string]ans
		ok          bool
		unsupported bool
		calls       []string
	}{
		{
			name:   "trace_block answers",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block": {resp: covPollerResult(`[{"type":"call"}]`)},
			},
			ok:    true,
			calls: []string{"trace_block"},
		},
		{
			name:   "debug_traceBlockByHash answers after trace_block is missing",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block":            {resp: notFound("trace_block")},
				"debug_traceBlockByHash": {resp: covPollerResult(`[{"result":{}}]`)},
			},
			ok:    true,
			calls: []string{"trace_block", "debug_traceBlockByHash"},
		},
		{
			name:   "parity replay answers last",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block":                   {err: common.NewErrEndpointUnsupported(errors.New("no"))},
				"debug_traceBlockByHash":        {resp: covPollerResult(`[]`)},
				"trace_replayBlockTransactions": {resp: covPollerResult(`[{"trace":[]}]`)},
			},
			ok:    true,
			calls: []string{"trace_block", "debug_traceBlockByHash", "trace_replayBlockTransactions"},
		},
		{
			name:   "all methods unsupported",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block":                   {resp: notFound("trace_block")},
				"debug_traceBlockByHash":        {err: common.NewErrUpstreamMethodIgnored("debug_traceBlockByHash", "u")},
				"trace_replayBlockTransactions": {resp: notFound("trace_replayBlockTransactions")},
			},
			unsupported: true,
			calls:       []string{"trace_block", "debug_traceBlockByHash", "trace_replayBlockTransactions"},
		},
		{
			name:   "mixed failures are not-available, not unsupported",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block":                   {resp: notFound("trace_block")},
				"debug_traceBlockByHash":        {err: errors.New("i/o timeout")},
				"trace_replayBlockTransactions": {resp: covPollerRpcError(-32000, "missing trie node")},
			},
			calls: []string{"trace_block", "debug_traceBlockByHash", "trace_replayBlockTransactions"},
		},
		{
			name:   "empty responses",
			header: covPollerBlock(9),
			answers: map[string]ans{
				"trace_block":                   {},
				"debug_traceBlockByHash":        {resp: covPollerResult(`null`)},
				"trace_replayBlockTransactions": {resp: covPollerResult(`[]`)},
			},
			calls: []string{"trace_block", "debug_traceBlockByHash", "trace_replayBlockTransactions"},
		},
		{
			name:   "block not found",
			header: covPollerResult(`null`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, up, ctx := covPollerNew(t, "cov-probe-trace", func(method string, _ []interface{}) (*common.NormalizedResponse, error) {
				if method == "eth_getBlockByNumber" {
					return tc.header, nil
				}
				a, ok := tc.answers[method]
				require.True(t, ok, "unexpected call to %s", method)
				return a.resp, a.err
			})
			ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeTraceData, 9)
			require.NoError(t, err)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.unsupported, unsupported)
			for _, m := range []string{"trace_block", "debug_traceBlockByHash", "trace_replayBlockTransactions"} {
				want := 0
				for _, c := range tc.calls {
					if c == m {
						want = 1
					}
				}
				assert.Equal(t, want, up.callCount(m), m)
			}
		})
	}

	t.Run("negative block", func(t *testing.T) {
		p, up, ctx := covPollerNew(t, "cov-probe-trace-neg", covPollerBlocks(1, 1))
		ok, unsupported, err := p.checkProbe(ctx, common.EvmProbeTraceData, -1)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Zero(t, up.callCount("eth_getBlockByNumber"))
	})
}
