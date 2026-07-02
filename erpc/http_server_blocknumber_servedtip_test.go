package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This suite pins eth_blockNumber semantics when the majority served tip is
// enabled for "latest" (EvmServedTipConfig). In that mode "latest"
// interpolation (resolveBlockTagToHex) anchors block-tagged methods — eth_call,
// eth_getLogs, ... — to the majority tip before forwarding, so eth_blockNumber
// must be pinned to that SAME tip: floor-only enforcement lets a fresher head
// (or a cache entry written from one) through, and clients observe
// eth_blockNumber AHEAD of the block "latest" actually executes at (an
// on-chain block.number read via eth_call comes back BELOW eth_blockNumber,
// even when the same upstream serves both calls).
//
// Topology (shared bni* helpers): rpc1 head 0x800, rpc2 head 0x1000. With two
// upstreams the majority tip is the LOWER head (evm.PickServedTip: N=2 → the
// 2nd-highest), so the served tip is 0x800 while rpc2 answers 0x1000.

// bniServedTipConfig is bniConfig (modern, no deprecated integrity block) with
// the majority served tip enabled for "latest".
func bniServedTipConfig(withCache bool) *common.Config {
	cfg := bniConfig(withCache, nil)
	cfg.Projects[0].Networks[0].Evm.ServedTip = &common.EvmServedTipConfig{EnabledFor: []string{"latest"}}
	return cfg
}

// bniServedTipBoot mirrors bniBoot with a caller-chosen upstream order and a
// wait condition for served-tip mode: max==0x1000 requires rpc2's poller and
// majority==0x800 requires BOTH pollers, so together they make the tip
// deterministic before any assertion runs.
func bniServedTipBoot(t *testing.T, cfg *common.Config, order ...string) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	*Network,
	func(),
) {
	t.Helper()
	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)

	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine, order...)

	ntw, err := prj.GetNetwork(context.Background(), util.EvmNetworkId(123))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		ctx := context.Background()
		return ntw.evmHighestBlockMax(ctx, false) == bniHealthyHead &&
			ntw.EvmHighestLatestBlockNumber(ctx) == bniLaggingHead
	}, 5*time.Second, 50*time.Millisecond,
		"pollers must learn both heads (majority tip=0x800, max=0x1000) before the scenario starts")

	return sendRequest, ntw, shutdown
}

func TestHttpServer_EthBlockNumberServedTipPin(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	t.Run("Bug_FreshResponseAboveTipMustBePinnedToServedTip", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// Fresh upstream first: rpc2 serves 0x1000, above the majority tip.
		send, _, shutdown := bniServedTipBoot(t, bniServedTipConfig(false), "rpc2", "rpc1")
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniLaggingHead, got,
			"eth_blockNumber must be pinned to the majority tip (0x%x) that \"latest\" interpolation uses, not the fresher upstream head (0x%x)",
			bniLaggingHead, bniHealthyHead)
	})

	t.Run("Bug_CacheHitAboveTipMustBePinnedToServedTip", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, ntw, shutdown := bniServedTipBoot(t, bniServedTipConfig(true), "rpc2", "rpc1")
		defer shutdown()

		// A fresh rpc2 response planted in the (possibly shared) cache — the
		// dominant path in practice: eth_blockNumber is realtime-cached, so
		// most reads are HITs carrying a value above the majority tip.
		bniSeedCache(t, ntw, "0x1000")

		got, hdrs := bniSend(t, send, nil, nil)
		t.Logf("response: block=0x%x cacheHeader=%q", got, hdrs["X-Erpc-Cache"])
		assert.Equal(t, bniLaggingHead, got,
			"a cache-hit response above the majority tip must be pinned down just like a fresh response")
	})

	t.Run("Baseline_ResponseAtServedTipPassesThrough", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// Lagging upstream first: rpc1 serves 0x800 == the majority tip.
		send, _, shutdown := bniServedTipBoot(t, bniServedTipConfig(false), "rpc1", "rpc2")
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniLaggingHead, got)
	})

	t.Run("Baseline_PerRequestOverrideServesRawValue", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniServedTipBoot(t, bniServedTipConfig(false), "rpc2", "rpc1")
		defer shutdown()

		// enforce-highest-block=false must bypass the pin exactly like it
		// bypasses max-mode enforcement.
		got, _ := bniSend(t, send, nil, map[string]string{"enforce-highest-block": "false"})
		assert.Equal(t, bniHealthyHead, got)
	})
}
