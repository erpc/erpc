package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unboundedAll drops the availability bound from every upstream, so each is
// called (and answers) for blocks above its polled head.
func unboundedAll(cfgs []*common.UpstreamConfig) {
	for _, cfg := range cfgs {
		cfg.Evm.BlockAvailability = nil
	}
}

// ethCallMock answers eth_call on host with body after delay.
func ethCallMock(host string, delay time.Duration, body string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == host && strings.Contains(util.SafeReadBody(r), "eth_call")
		}).
		Reply(200).
		Delay(delay).
		JSON([]byte(body))
}

// countEthCalls counts eth_call requests reaching host, ahead of its standard
// mock.
func countEthCalls(host string, hits *atomic.Int64) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			// Filters run before host matching.
			if r.URL.Host == host && strings.Contains(util.SafeReadBody(r), "eth_call") {
				hits.Add(1)
			}
			return false
		}).
		Reply(200)
}

const missingDataBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`

// hedgeFaster hedges well before a slow upstream answers.
func hedgeFaster() []*common.FailsafeConfig {
	return []*common.FailsafeConfig{{
		MatchMethod: "*",
		Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(20 * time.Millisecond), MaxCount: 1},
	}}
}

func forwardEthCall(t *testing.T, ctx context.Context, network *Network, id int, blockHex string) (string, error) {
	t.Helper()
	req := ethCallRequest(id, blockHex)
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	if err != nil {
		return "", err
	}
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	return strings.Trim(jrr.GetResultString(), `"`), nil
}

// A hedge leg that finds every routed upstream missing the data must not
// cancel a sibling leg that escalated to the fallbacks and is still waiting.
func TestFailover_HedgeKeepsEscalatedSibling(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The primaries miss, the escape sends one leg to the (slow) fallbacks,
	// and the hedge leg misses on the primaries again.
	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x3e8", // 1000
		fallbackLatest: "0x3e8", // 1000
		enableFailover: true,
		configure:      unboundedAll,
		failsafe:       hedgeFaster(),
		mocks: func() {
			ethCallMock("rpc1.localhost", 0, missingDataBody)
			ethCallMock("rpc2.localhost", 0, missingDataBody)
			ethCallMock("rpc3.localhost", 80*time.Millisecond, `{"jsonrpc":"2.0","id":1,"result":"0x3333"}`)
			ethCallMock("rpc4.localhost", 80*time.Millisecond, `{"jsonrpc":"2.0","id":1,"result":"0x3333"}`)
		},
	})

	escape := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
	escapeBefore := promUtil.ToFloat64(escape)
	for i := 0; i < 5; i++ {
		result, err := forwardEthCall(t, ctx, network, i, "0x3ea")
		require.NoError(t, err, "iter %d", i)
		assert.Equal(t, "0x3333", result, "iter %d", i)
	}
	assert.Equal(t, escapeBefore+5, promUtil.ToFloat64(escape))
}

// A fallback that already answered missing data must not let a hedge leg
// cancel another fallback that is still working on the request.
func TestFailover_HedgeKeepsSlowFallbackAfterFastFallbackMiss(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x3e8", // 1000
		fallbackLatest: "0x3e8", // 1000
		enableFailover: true,
		configure:      unboundedAll,
		failsafe:       hedgeFaster(),
		mocks: func() {
			ethCallMock("rpc1.localhost", 0, missingDataBody)
			ethCallMock("rpc2.localhost", 0, missingDataBody)
			ethCallMock("rpc3.localhost", 0, missingDataBody)
			ethCallMock("rpc4.localhost", 80*time.Millisecond, `{"jsonrpc":"2.0","id":1,"result":"0x4444"}`)
		},
	})

	for i := 0; i < 5; i++ {
		result, err := forwardEthCall(t, ctx, network, i, "0x3ea")
		require.NoError(t, err, "iter %d", i)
		assert.Equal(t, "0x4444", result, "iter %d", i)
	}
}

// A fallback whose polled head is ahead does not take reads the primaries
// serve: fallbacks are reached only through the escape. (The default
// policy's probeExcluded may still mirror the request to them in the
// background, so upstream hits are not counted here.)
func TestFailover_FallbackAheadDoesNotTakeServedReads(t *testing.T) {
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x3e8", // 1000
		fallbackLatest: "0x3ea", // 1002
		enableFailover: true,
		configure:      unboundedAll,
	})

	escape := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
	escapeBefore := promUtil.ToFloat64(escape)
	for i := 0; i < 5; i++ {
		result, err := forwardEthCall(t, ctx, network, i, "0x3ea")
		require.NoError(t, err, "iter %d", i)
		assert.Contains(t, []string{"0x1111", "0x2222"}, result, "iter %d", i)
	}
	assert.Equal(t, escapeBefore, promUtil.ToFloat64(escape))
}
