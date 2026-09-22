package integrity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real captured responses, run through the EXACT check set a deployment runs
// for that chain — CheckSetForLevel + ApplyChainProfile, no hand-picked subset.
//
// Every other test in this package feeds the catalog hand-written JSON, which
// only ever proves the checks do what their author expected. It cannot catch
// the failure that actually costs money: a check whose premise is true of the
// blocks someone sampled and false of the blocks a chain really serves. That is
// exactly how headerConsensusInvariants came to reject every pre-merge Ethereum
// block — difficulty is zero on every block anyone measured, and non-zero on
// the seven years of history before The Merge.
//
// So the fixtures deliberately straddle each chain's hard fork and include its
// genesis, its emptiest blocks and its busiest ones. A chain profile that is
// wrong about any era shows up here as a violation on a response a real node
// really returned.
//
// Refreshing them: scripts capture {method, params, result} verbatim from a
// public node. Old heights are immutable, so only the "recent" fixtures ever
// need re-pinning.

type realFixture struct {
	Method string          `json:"method"`
	Params []any           `json:"params"`
	Result json.RawMessage `json:"result"`
}

func loadRealFixtures(t *testing.T, chainDir string) map[string]realFixture {
	t.Helper()
	dir := filepath.Join("testdata", chainDir)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "fixture directory missing")
	out := map[string]realFixture{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		var fx realFixture
		require.NoError(t, json.Unmarshal(raw, &fx), "fixture %s", e.Name())
		out[strings.TrimSuffix(e.Name(), ".json")] = fx
	}
	require.NotEmpty(t, out, "no fixtures found in %s", dir)
	return out
}

// validateFixture runs one captured response through the chain's resolved check
// set, exactly as the request path does.
func validateFixture(t *testing.T, chainId int64, level Level, fx realFixture) Result {
	t.Helper()
	cs := CheckSetForLevel(level)
	ApplyChainProfile(cs, chainId)

	params, err := json.Marshal(fx.Params)
	require.NoError(t, err)
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, fx.Method, params)))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), fx.Result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	return Validate(context.Background(), Input{
		Reorg:    DefaultReorgPolicy(),
		Method:   fx.Method,
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Params:   fx.Params,
	})
}

// No honest Arbitrum One response may be rejected — on either side of the Nitro
// migration (block 22207817), which changes two of the three header fields
// headerConsensusInvariants knows about:
//
//	                sha3Uncles      difficulty   nonce
//	classic  (<N)   empty-ommers    0x0          0x0
//	nitro    (>=N)  empty-ommers    0x1          non-zero, incrementing
//
// The profile declares EmptyUncles and nothing else, which is the only claim
// true of both eras. Declaring zeroDifficulty or zeroNonce from classic-era
// data would reject every block since 2022; declaring them from Nitro data
// would reject everything before it. This test is what keeps either from being
// added by someone who sampled one era.
func TestRealArbitrumFixturesAreNeverRejected(t *testing.T) {
	const arbitrumOne = 42161
	fixtures := loadRealFixtures(t, "arbitrum")

	names := make([]string, 0, len(fixtures))
	for n := range fixtures {
		names = append(names, n)
	}
	sort.Strings(names)

	// Which checks actually verified something, across the whole corpus. A
	// suite where everything skips proves nothing, so the coverage assertions
	// below are part of the test, not diagnostics.
	verified := map[string]int{}

	for _, name := range names {
		fx := fixtures[name]
		t.Run(name, func(t *testing.T) {
			res := validateFixture(t, arbitrumOne, LevelCorroborated, fx)
			require.NoError(t, res.Err,
				"honest Arbitrum response rejected by %q: %v", res.RejectedCheckID, res.Err)
			assert.Empty(t, res.Recorded, "honest Arbitrum response soft-flagged")
			for _, oc := range res.Outcomes {
				if oc.Outcome == "pass" {
					verified[oc.CheckID]++
				}
			}
		})
	}

	// Checks this corpus is known to exercise. Asserting them keeps the suite
	// honest: without it, a profile change that silently turned everything into
	// a skip would still show 29 green subtests while proving nothing.
	//
	// The list is what the corpus MEASURABLY verifies, not a wish list — the
	// header/identity/structural checks from the block fixtures, and the
	// bloom/log/receipt checks from the eth_getBlockReceipts fixtures, which
	// cover the classic era too.
	for _, id := range []string{
		"schemaConformance",
		"headerConsensusInvariants",
		"headerFieldShapes",
		"transactionsRootConsistency",
		"txBlockInfo",
		"txFieldUniqueness",
		"blockByNumberIdentity",
		"blockByHashIdentity",
		"bloomMatch",
		"bloomEmptiness",
		"logIndexContiguity",
		"logMetadata",
		"logFieldShapes",
		"indexMagnitude",
		"sameBlockHash",
		"txHashUniqueness",
		"transactionIndexConsistency",
		"senderRecovery",
		"txByHashIdentity",
	} {
		assert.NotZero(t, verified[id], "%s verified nothing across the whole corpus", id)
	}
}

// Arbitrum headers carry l1BlockNumber (both eras) plus sendCount and sendRoot
// (Nitro), none of them fields the reference encoder knows — so the block-hash
// recompute cannot run on this chain at all. That must be visible as a SKIP.
// Reporting it as a pass would tell an operator the strongest check in the
// catalog is green on a chain where it has never verified a single block.
func TestRealArbitrumBlockHashRecomputeHonestlySkips(t *testing.T) {
	fixtures := loadRealFixtures(t, "arbitrum")
	seen := 0
	for name, fx := range fixtures {
		if !strings.HasPrefix(name, "block-") && !strings.HasPrefix(name, "blockbyhash-") {
			continue
		}
		res := validateFixture(t, 42161, LevelCorroborated, fx)
		if oc := outcomeOf(res, "blockHashRecompute"); oc != "" {
			seen++
			assert.Equal(t, "skip", oc,
				"%s: blockHashRecompute cannot verify an Arbitrum header, so it must not report a pass", name)
		}
	}
	assert.NotZero(t, seen, "no block fixtures exercised blockHashRecompute")
}

// The recompute family is disabled for this architecture because ArbOS commits
// system transactions the RPC response omits, so a root recomputed from the
// response can never reproduce the header's. Asserted on the resolved set
// rather than on behaviour: it must be OFF, not merely skipping.
func TestArbitrumProfileDisablesRecomputeFamily(t *testing.T) {
	cs := CheckSetForLevel(LevelAuthoritative)
	ApplyChainProfile(cs, 42161)
	for _, id := range recomputeFamily {
		_, ok := cs[id]
		assert.False(t, ok, "%s must be disabled for arbitrum-nitro", id)
	}
	// ...while the checks that DO work there stay enabled.
	for _, id := range []string{"blockHashRecompute", "headerConsensusInvariants", "getLogsFilterSanity"} {
		_, ok := cs[id]
		assert.True(t, ok, "%s should remain enabled for arbitrum", id)
	}
}
