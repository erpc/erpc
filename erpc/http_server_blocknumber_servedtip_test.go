package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This suite pins eth_blockNumber semantics on the default config. "latest"
// interpolation (resolveBlockTagToHex) anchors block-tagged methods — eth_call,
// eth_getLogs, ... — to the served head before forwarding, so eth_blockNumber
// must be pinned to that SAME head: floor-only enforcement lets a fresher head
// (or a cache entry written from one) through, and clients observe
// eth_blockNumber AHEAD of the block "latest" actually executes at (an
// on-chain block.number read via eth_call comes back BELOW eth_blockNumber,
// even when the same upstream serves both calls).
//
// Topology (shared bni* helpers): rpc1 head 0x800, rpc2 head 0x1000. With two
// upstreams the default quorum (2) serves the LOWER head, so the served head is
// 0x800 while rpc2 answers 0x1000.

// bniServedTipBoot mirrors bniBoot with a caller-chosen upstream order and a
// wait condition for the two-upstream topology: every head known with
// max==0x1000, and the served head 0x800, so the head is deterministic before
// any assertion runs.
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
		ceiling, known := ntw.futureBlockCeiling(ctx)
		return known && ceiling == bniHealthyHead &&
			ntw.EvmHighestLatestBlockNumber(ctx) == bniLaggingHead
	}, 5*time.Second, 50*time.Millisecond,
		"pollers must learn both heads (served=0x800, max=0x1000) before the scenario starts")

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

		// Fresh upstream first: rpc2 serves 0x1000, above the served tip.
		send, _, shutdown := bniServedTipBoot(t, bniConfig(false, nil), "rpc2", "rpc1")
		defer shutdown()

		got, hdrs := bniSend(t, send, nil, nil)
		assert.Equal(t, bniLaggingHead, got,
			"eth_blockNumber must be pinned to the served tip (0x%x) that \"latest\" interpolation uses, not the fresher upstream head (0x%x)",
			bniLaggingHead, bniHealthyHead)
		assert.Empty(t, hdrs["X-Erpc-Upstream"],
			"the pin discards any upstream answer, so the served tip must be answered without dispatching")
	})

	t.Run("Bug_CacheHitAboveTipMustBePinnedToServedTip", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, ntw, shutdown := bniServedTipBoot(t, bniConfig(true, nil), "rpc2", "rpc1")
		defer shutdown()

		// A fresh rpc2 response planted in the (possibly shared) cache — the
		// dominant path in practice: eth_blockNumber is realtime-cached, so
		// most reads are HITs carrying a value above the served tip.
		bniSeedCache(t, ntw, "0x1000")

		got, hdrs := bniSend(t, send, nil, nil)
		t.Logf("response: block=0x%x cacheHeader=%q", got, hdrs["X-Erpc-Cache"])
		assert.Equal(t, bniLaggingHead, got,
			"a cache-hit response above the served tip must be pinned down just like a fresh response")
	})

	t.Run("Baseline_ResponseAtServedTipPassesThrough", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// Lagging upstream first: rpc1 serves 0x800 == the served tip.
		send, _, shutdown := bniServedTipBoot(t, bniConfig(false, nil), "rpc1", "rpc2")
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		assert.Equal(t, bniLaggingHead, got)
	})

	t.Run("Baseline_PerRequestOverrideServesRawValue", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniServedTipBoot(t, bniConfig(false, nil), "rpc2", "rpc1")
		defer shutdown()

		// enforce-highest-block=false must bypass the pin exactly like it
		// bypasses max-mode enforcement.
		got, _ := bniSend(t, send, nil, map[string]string{"enforce-highest-block": "false"})
		assert.Equal(t, bniHealthyHead, got)
	})

	t.Run("LocalAnswerRefreshesBallotPollers", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Heads move up 5 blocks once `moved` flips. Registered before the
		// static bootstrap mocks so gock matches them first; until the flip
		// they decline and the static heads (0x800/0x1000) answer.
		var moved atomic.Bool
		for host, head := range map[string]string{
			"http://rpc1.localhost": "0x805",
			"http://rpc2.localhost": "0x1005",
		} {
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return moved.Load() && strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
				}).
				Reply(200).
				JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8f0"}}`, head)))
		}
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// A 30s ticker cannot poll during the test; a 100ms debounce lets each
		// eth_blockNumber answer kick a refresh almost at once.
		cfg := bniConfig(false, nil)
		for _, u := range cfg.Projects[0].Upstreams {
			u.Evm.StatePollerInterval = common.Duration(30 * time.Second)
			u.Evm.StatePollerDebounce = common.Duration(100 * time.Millisecond)
		}
		send, _, shutdown := bniServedTipBoot(t, cfg, "rpc2", "rpc1")
		defer shutdown()

		got, _ := bniSend(t, send, nil, nil)
		require.Equal(t, bniLaggingHead, got)

		moved.Store(true)
		require.Eventually(t, func() bool {
			got, _ := bniSend(t, send, nil, nil)
			return got == bniLaggingHead+5
		}, 3*time.Second, 50*time.Millisecond,
			"eth_blockNumber must refresh the ballot's pollers and reach the moved served head (0x%x) without waiting for the 30s poll interval",
			bniLaggingHead+5)
	})
}
