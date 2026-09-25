package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// Priority-tier forwarding tests. "paid" is registered FIRST (so the
// pinned/policy order would pick it) with a higher priority number;
// "cheap" has priority 0. Priority must win over the policy order.

const prioReq = `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`

func prioMock(host string, times int) *gock.Response {
	return gock.New("http://" + host).Post("").Times(times).
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBalance") }).
		Reply(200)
}

func prioNetwork(t *testing.T, ctx context.Context, cheapPrio, paidPrio *int, fs *common.FailsafeConfig, cheapMut func(*common.UpstreamConfig)) *Network {
	t.Helper()
	paid := &common.UpstreamConfig{
		Type: common.UpstreamTypeEvm, Id: "paid", Endpoint: "http://rpc1.localhost",
		Evm:     &common.EvmUpstreamConfig{ChainId: 123},
		Routing: &common.UpstreamRoutingConfig{Priority: paidPrio},
	}
	cheap := &common.UpstreamConfig{
		Type: common.UpstreamTypeEvm, Id: "cheap", Endpoint: "http://rpc2.localhost",
		Evm:     &common.EvmUpstreamConfig{ChainId: 123},
		Routing: &common.UpstreamRoutingConfig{Priority: cheapPrio},
	}
	if cheapMut != nil {
		cheapMut(cheap)
	}
	if fs == nil {
		fs = &common.FailsafeConfig{}
	}
	nc := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe:     []*common.FailsafeConfig{fs},
	}
	n := setupTestNetwork(t, ctx, []*common.UpstreamConfig{paid, cheap}, nc)
	// Policy order puts paid first (as if it scored best).
	n.PinUpstreamOrderForTest("paid", "cheap")
	return n
}

func forwardPrio(t *testing.T, ctx context.Context, n *Network) (*common.NormalizedResponse, error) {
	t.Helper()
	return n.Forward(ctx, common.NewNormalizedRequest([]byte(prioReq)))
}

func TestNetwork_PriorityTiers(t *testing.T) {
	ok := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"}

	t.Run("HealthyCheapExcludesPaid_PriorityBeatsOrder", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		prioMock("rpc2.localhost", 5).JSON(ok)
		paid := prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), nil, nil)
		for i := 0; i < 5; i++ {
			resp, err := forwardPrio(t, ctx, n)
			require.NoError(t, err)
			require.Equal(t, "cheap", resp.Upstream().Id())
		}
		require.False(t, paid.Mock.Done(), "paid tier must not be called while cheap is healthy")
	})

	t.Run("CheapFailureFallsBackSameRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		prioMock("rpc2.localhost", 1).Status(429).JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": -32005, "message": "rate limited"}})
		prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), nil, nil)
		resp, err := forwardPrio(t, ctx, n)
		require.NoError(t, err)
		require.Equal(t, "paid", resp.Upstream().Id())
	})

	t.Run("RevertDoesNotEscalate", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		prioMock("rpc2.localhost", 1).JSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": 3, "message": "execution reverted"}})
		paid := prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), nil, nil)
		_, err := forwardPrio(t, ctx, n)
		require.Error(t, err)
		require.False(t, paid.Mock.Done(), "deterministic revert must not spend on the paid tier")
	})

	t.Run("UnavailableCheapSkipped", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		cheap := prioMock("rpc2.localhost", 1).JSON(ok)
		prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), nil, func(c *common.UpstreamConfig) {
			c.IgnoreMethods = []string{"eth_getBalance"}
		})
		resp, err := forwardPrio(t, ctx, n)
		require.NoError(t, err)
		require.Equal(t, "paid", resp.Upstream().Id())
		require.False(t, cheap.Mock.Done())
	})

	t.Run("NoPriorityParity", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		prioMock("rpc2.localhost", 1).JSON(ok)
		prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// nil vs explicit 0: one tier, so the policy's own order is used verbatim.
		n := prioNetwork(t, ctx, nil, util.IntPtr(0), nil, nil)
		resp, err := forwardPrio(t, ctx, n)
		require.NoError(t, err)
		require.Equal(t, "paid", resp.Upstream().Id(), "no tiers: policy order untouched")
	})

	t.Run("HedgeDoesNotCrossIntoPaidTier", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		prioMock("rpc2.localhost", 1).Delay(400 * time.Millisecond).JSON(ok)
		paid := prioMock("rpc1.localhost", 1).JSON(ok)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), &common.FailsafeConfig{
			Hedge: &common.HedgePolicyConfig{Delay: common.NewStaticDuration(50 * time.Millisecond), MaxCount: 2},
		}, nil)
		resp, err := forwardPrio(t, ctx, n)
		require.NoError(t, err)
		require.Equal(t, "cheap", resp.Upstream().Id())
		require.False(t, paid.Mock.Done(), "hedge must stay inside the cheapest tier")
	})

	t.Run("RetryBudgetBounded", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		fail := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": -32603, "message": "internal"}}
		cheap := prioMock("rpc2.localhost", 10).Status(500).JSON(fail)
		paid := prioMock("rpc1.localhost", 10).Status(500).JSON(fail)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := prioNetwork(t, ctx, util.IntPtr(0), util.IntPtr(10), &common.FailsafeConfig{
			Retry: &common.RetryPolicyConfig{MaxAttempts: 2},
		}, nil)
		_, err := forwardPrio(t, ctx, n)
		require.Error(t, err)
		// MaxAttempts(2) x one sweep over 2 tiers → at most 2 calls each.
		require.LessOrEqual(t, 10-cheap.Mock.Request().Counter, 2)
		require.LessOrEqual(t, 10-paid.Mock.Request().Counter, 2)
	})
}
