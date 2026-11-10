package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// Topology: every head poll (the hash votes the served head is learned from)
// reports 0x800, while rpc2's own eth_blockNumber answer runs ahead at 0x1000,
// as a node's answer does once a block arrived after the last head fetch. The
// served head is 0x800 while rpc2 answers 0x1000.

// stpHeads registers, ahead of setupBniPollerMocks' static heads, every
// upstream's latest header at head(), and each block below it by number:
// hash and parentHash derived from the number, so each poll is a hash vote
// of one chain, and a tracker that walks a jump finds the blocks it skipped.
func stpHeads(head func() int64) {
	header := func(n int64) string {
		return fmt.Sprintf(`{"result":{"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x6702a8f0"}}`, n, n, n-1)
	}
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost", "http://rpc3.localhost"} {
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				b := util.SafeReadBody(r)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				res.Body = io.NopCloser(strings.NewReader(header(head())))
				return res
			})
		gock.New(host).Post("").Persist().
			Filter(func(r *http.Request) bool {
				n, ok := stpByNumber(util.SafeReadBody(r))
				return ok && n <= head()
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				n, _ := stpByNumber(util.SafeReadBody(res.Request))
				res.Body = io.NopCloser(strings.NewReader(header(n)))
				return res
			})
	}
}

// stpByNumber reads the block number of an eth_getBlockByNumber request
// for a numbered block.
func stpByNumber(body string) (int64, bool) {
	var req struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	if json.Unmarshal([]byte(body), &req) != nil || req.Method != "eth_getBlockByNumber" || len(req.Params) == 0 {
		return 0, false
	}
	ref, _ := req.Params[0].(string)
	n, err := common.HexToInt64(ref)
	return n, err == nil && strings.HasPrefix(ref, "0x")
}

// bniServedTipBoot mirrors bniBoot with a caller-chosen upstream order and
// waits for the served head 0x800, so it is deterministic before any
// assertion runs.
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
		return servedHead(ntw, context.Background()).Latest == bniLaggingHead
	}, 5*time.Second, 50*time.Millisecond,
		"pollers must learn the served head (0x800) before the scenario starts")

	return sendRequest, ntw, shutdown
}

func TestHttpServer_EthBlockNumberServedTipPin(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	t.Run("Bug_FreshResponseAboveTipMustBePinnedToServedTip", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		stpHeads(func() int64 { return bniLaggingHead })
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

		// erigon_blockNumber is the same question (spec §7 family 1).
		status, ehdrs, body := send(`{"jsonrpc":"2.0","id":1,"method":"erigon_blockNumber","params":[]}`, nil, nil)
		require.Equal(t, http.StatusOK, status, body)
		assert.Contains(t, body, fmt.Sprintf(`"result":"0x%x"`, bniLaggingHead),
			"erigon_blockNumber must answer the served tip, not an upstream's head")
		assert.Empty(t, ehdrs["X-Erpc-Upstream"], "erigon_blockNumber is answered without dispatching")
	})

	t.Run("Bug_CacheHitAboveTipMustBePinnedToServedTip", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		stpHeads(func() int64 { return bniLaggingHead })
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
		t.Cleanup(util.ResetGock)
		stpHeads(func() int64 { return bniLaggingHead })
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
		t.Cleanup(util.ResetGock)
		stpHeads(func() int64 { return bniLaggingHead })
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		send, _, shutdown := bniServedTipBoot(t, bniConfig(false, nil), "rpc2", "rpc1")
		defer shutdown()

		// enforce-highest-block=false must bypass the pin exactly like it
		// bypasses max-mode enforcement.
		got, _ := bniSend(t, send, nil, map[string]string{"enforce-highest-block": "false"})
		assert.Equal(t, bniHealthyHead, got)
	})

	t.Run("LatestReadsMoveTheServedHead", func(t *testing.T) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)

		// Heads move up 5 blocks once `moved` flips.
		var moved atomic.Bool
		stpHeads(func() int64 {
			if moved.Load() {
				return bniLaggingHead + 5
			}
			return bniLaggingHead
		})
		setupBniPollerMocks()
		setupBniBlockNumberMocks()

		// A 30s ticker cannot poll during the test: the reads are the block
		// clock's demand, and a 100ms debounce paces its early retries.
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
			"latest reads must move the served head to 0x%x without waiting for the 30s poll interval",
			bniLaggingHead+5)
	})
}
