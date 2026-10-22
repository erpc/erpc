package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
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
// Topology (shared bni* helpers): rpc1 head 0x800, rpc2 head 0x1000. The pair
// is 2048 blocks apart, past the default maxRegressionBlocks (1024), so the
// pair rule does not apply and the default quorum (2) serves the LOWER head:
// the served head is 0x800 while rpc2 answers 0x1000.

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
		// rpc3 is a static serving range (upper.exactBlock below its head), first
		// in routing order: it never votes, so it must not take a refresh slot
		// from the voters rpc2 and rpc1.
		rangeEnd := int64(0x700)
		cfg.Projects[0].Upstreams[2].Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
			Upper: &common.EvmAvailabilityBoundConfig{ExactBlock: &rangeEnd},
		}
		send, _, shutdown := bniServedTipBoot(t, cfg, "rpc3", "rpc2", "rpc1")
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

	// erpc#1253: two upstreams a few blocks apart (one behind by less than the
	// plausible lead, so it is never benched). The pick used to be the lower
	// head, so eth_blockNumber and every "latest" interpolation sat on the
	// lagging peer for as long as it kept up — while a direct call to the
	// fresher upstream, and eth_blockNumber with the pin bypassed, showed the
	// real tip. The pair rule serves the higher head, and eth_call("latest")
	// executes at that same height (the Multicall3 getBlockNumber signal from
	// the issue: call_lag = tip - call_block must be 0).
	t.Run("Bug_StaggeredPairTracksTheFresherHead", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		const lagging, fresh = int64(0x1000), int64(0x1003)
		var callBlock atomic.Value
		for host, head := range map[string]string{
			"http://rpc1.localhost": "0x1000",
			"http://rpc2.localhost": "0x1003",
		} {
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					return strings.Contains(util.SafeReadBody(r), "eth_chainId")
				}).
				Reply(200).
				JSON([]byte(`{"result":"0x7b"}`))
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					return strings.Contains(util.SafeReadBody(r), "eth_syncing")
				}).
				Reply(200).
				JSON([]byte(`{"result":false}`))
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
				}).
				Reply(200).
				JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8f0"}}`, head)))
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
				}).
				Reply(200).
				JSON([]byte(`{"result":{"number":"0x900","timestamp":"0x6702a8e0"}}`))
			gock.New(host).Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					if !strings.Contains(b, "eth_call") {
						return false
					}
					var req struct {
						Params []any `json:"params"`
					}
					if err := sonic.UnmarshalString(b, &req); err == nil && len(req.Params) == 2 {
						callBlock.Store(req.Params[1])
					}
					return true
				}).
				Reply(200).
				JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x0000000000000000000000000000000000000000000000000000000000001003"}`))
		}

		cfg := bniConfig(false, nil)
		cfg.Projects[0].Upstreams = cfg.Projects[0].Upstreams[:2]
		send, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()
		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		// The lagging upstream first, so the pin is what keeps the answer honest.
		policy.OverrideAllForTest(prj.policyEngine, "rpc1", "rpc2")
		ntw, err := prj.GetNetwork(context.Background(), util.EvmNetworkId(123))
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			ceiling, known := ntw.futureBlockCeiling(context.Background())
			return known && ceiling == fresh
		}, 5*time.Second, 50*time.Millisecond, "pollers must learn both heads before the scenario starts")

		got, _ := bniSend(t, send, nil, nil)
		require.Equal(t, fresh, got,
			"eth_blockNumber must track the fresher head of a plausible pair (0x%x), not pin to the lagging peer (0x%x)", fresh, lagging)

		status, _, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x42cbb15c"},"latest"]}`, nil, nil)
		require.Equal(t, http.StatusOK, status, "body: %s", body)
		require.Equal(t, fmt.Sprintf("0x%x", got), callBlock.Load(),
			"eth_call(\"latest\") must execute at the height eth_blockNumber reports")
	})
}
