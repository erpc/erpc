package consensus

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headNetwork is a minimal EvmNetwork test double reporting a fixed "latest"
// head, so the empty-outside-retention waiver's block proof can be exercised
// end-to-end through determineWinner — the path chainHead actually reads
// (common.EvmHighestLatestBlockNumber). The unit tests inject head/winnerBlock
// directly; these drive the whole determineWinner pipeline the way a live
// round does, which is what the reviewer asked to see proven.
type headNetwork struct {
	common.Network
	latest  int64
	maxMode bool // true → Config has no served-tip majority; chainHead must not trust latest
}

func (n *headNetwork) Id() string                                           { return "evm:1" }
func (n *headNetwork) Label() string                                        { return "evm:1" }
func (n *headNetwork) ProjectId() string                                    { return "test-project" }
func (n *headNetwork) Architecture() common.NetworkArchitecture             { return common.ArchitectureEvm }
func (n *headNetwork) EvmHighestLatestBlockNumber(context.Context) int64    { return n.latest }
func (n *headNetwork) EvmHighestFinalizedBlockNumber(context.Context) int64 { return 0 }
func (n *headNetwork) EvmLeaderUpstream(context.Context) common.Upstream    { return nil }
func (n *headNetwork) EvmRefreshLatestBlock(context.Context)               {}

// Without this, a new EvmNetwork method makes the fake silently stop being an
// EvmNetwork and chainHead falls back to participant pollers (fail-closed).
var _ common.EvmNetwork = (*headNetwork)(nil)

func (n *headNetwork) Config() *common.NetworkConfig {
	if n.maxMode {
		return &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}
	}
	return &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			ServedTip: &common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority},
		},
	}
}

// winnerWithHead runs the full determineWinner pipeline with a network that
// reports `head` as its latest block, so both the tie-dispute rule and the
// waiver enforcement (chainHead + requestBlock) see production-shaped inputs.
// block > 0 makes the client request name that block; 0 sends no params.
func winnerWithHead(t *testing.T, cfg *config, method string, block, head int64, results []*execResult) *slotResult {
	t.Helper()
	return winnerOnNetwork(t, cfg, method, block, &headNetwork{latest: head}, results)
}

func winnerOnNetwork(t *testing.T, cfg *config, method string, block int64, net *headNetwork, results []*execResult) *slotResult {
	t.Helper()
	lg := zerolog.Nop()
	ctx := requestCtx(method, block, net)
	analysis := newConsensusAnalysis(&lg, ctx, cfg, results)
	require.False(t, analysis.hasRemaining(), "test must present a complete round")
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	return e.determineWinner(ctx, &lg, analysis)
}

func requestCtx(method string, block int64, net *headNetwork) context.Context {
	rpc := &common.JsonRpcRequest{Method: method}
	if block > 0 {
		rpc.Params = []interface{}{fmt.Sprintf("0x%x", block), false}
	}
	req := common.NewNormalizedRequestFromJsonRpcRequest(rpc)
	req.SetNetwork(net)
	return context.WithValue(context.Background(), common.RequestContextKey, req)
}

// waivedMetricValue reads the current empty_outside_retention waiver counter
// for the test project/network/tag so a test can assert the exact delta a round
// produced (the counter is a package global other tests also touch).
func waivedMetricValue() float64 {
	return testutil.ToFloat64(
		telemetry.MetricConsensusCompositionWaived.WithLabelValues(
			"test-project", "evm:1", "type:internal", waiverReasonEmptyOutsideRetention),
	)
}

// TestEmptyOutsideRetentionWaiver_PromotesLoneNonEmpty is the end-to-end proof
// that, driven through determineWinner with a live EvmNetwork head and a
// request naming its block, the empty-outside-retention waiver promotes the
// agreeing archives over pruned internals and emits
// erpc_consensus_composition_waived_total{reason=empty_outside_retention}.
func TestEmptyOutsideRetentionWaiver_PromotesLoneNonEmpty(t *testing.T) {
	internal1 := taggedUpstream("internal-1", "type:internal")
	internal2 := taggedUpstream("internal-2", "type:internal")
	ext1 := taggedUpstream("external-1", "type:external")
	ext2 := taggedUpstream("external-2", "type:external")

	head := int64(20_000_000)
	// Block 46147 — depth ~20M, far past retention 100.
	oldBlock := int64(46_147)
	// Block 19_999_920: depth 80 <= retention 100 → inside range.
	recentBlock := int64(19_999_920)
	tx := map[string]interface{}{"number": "0xb443"}
	method := "eth_getBlockByNumber"

	newCfg := func() *config {
		return &config{
			maxParticipants:      4,
			agreementThreshold:   2,
			requiredParticipants: emptyWaiverQuota(),
		}
	}

	// Case 1: 1 empty internal + 2 agreeing archives. The archives are the
	// unique majority; the waiver releases the internal quota so the round
	// succeeds instead of composition-disputing.
	t.Run("1 empty internal + 2 agreeing archives", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		winner := winnerWithHead(t, cfg, method, oldBlock, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.Nil(t, winner.Error, "the waiver must release the pruned internal quota, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	// Case 1b: 1 empty internal + 1 internal transport error + 2 archives.
	// minAgreement:1 — a down sibling must not block the empty waiver.
	t.Run("1 empty internal + sibling transport error + 2 archives", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 4
		before := waivedMetricValue()

		winner := winnerWithHead(t, cfg, method, oldBlock, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			errorFrom(internal2, common.NewErrEndpointTransportFailure(&url.URL{Host: "internal-2"}, fmt.Errorf("connection refused")), 1),
			objResultFrom(t, ext1, tx, 2),
			objResultFrom(t, ext2, tx, 3),
		})

		require.NotNil(t, winner)
		require.Nil(t, winner.Error, "sibling transport error must not block minAgreement empty waiver, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	// Case 1c: short bag — maxParticipants 4 but only 3 answers. Explicit
	// seal (after every slot acknowledged) lets the empty waiver evaluate;
	// wait-cap cancel alone must not seal.
	t.Run("1 empty internal + 2 archives with sealed short bag", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 4
		before := waivedMetricValue()

		lg := zerolog.Nop()
		ctx := requestCtx(method, oldBlock, &headNetwork{latest: head})
		analysis := newConsensusAnalysis(&lg, ctx, cfg, []*execResult{
			emptyResultFrom(t, internal1, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})
		require.True(t, analysis.hasRemaining())
		analysis.sealCollection()
		require.False(t, analysis.hasRemaining())

		e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
		winner := e.determineWinner(ctx, &lg, analysis)

		require.NotNil(t, winner)
		require.Nil(t, winner.Error, "sealed short bag must allow empty waiver at mp=4, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	// Case 2: 2 empty internals + 2 agreeing archives — both groups sit at
	// agreementThreshold 2. This is the default mixed-node shape and the exact
	// case the reviewer found untested: the rules engine synthesizes a
	// tie-dispute with no backing group, so before the fix the waiver was never
	// consulted. promoteAbstentionWinner rescues the lone non-empty group.
	t.Run("2 empty internals + 2 agreeing archives (balanced tie)", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()

		winner := winnerWithHead(t, cfg, method, oldBlock, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			emptyResultFrom(t, internal2, 1),
			objResultFrom(t, ext1, tx, 2),
			objResultFrom(t, ext2, tx, 3),
		})

		require.NotNil(t, winner)
		require.Nil(t, winner.Error,
			"the balanced empty-vs-archive tie must resolve to the archives via the waiver, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	// Guard: a recent block (inside retention) must NOT be rescued. Two empty
	// internals could legitimately be right — the data may simply not exist yet
	// — so the tie must stand as a dispute. This is the anti-fabrication floor
	// that stops correlated externals outvoting an internal on live data.
	t.Run("balanced tie holds when requested block is inside retention", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()

		winner := winnerWithHead(t, cfg, method, recentBlock, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			emptyResultFrom(t, internal2, 1),
			objResultFrom(t, ext1, tx, 2),
			objResultFrom(t, ext2, tx, 3),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "a recent-block tie must not be waived")
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute),
			"an in-retention empty tie stays a dispute, got: %v", winner.Error)
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire inside retention")
	})

	// Live mixed-node rounds can have working poller latests even when the
	// network served tip is still 0 (cold start, or the request selector scoped
	// the gather away from the tip). The waiver proves "outside retention" from
	// participant pollers when at least one non-winner (here the empty
	// internal) corroborates the head.
	t.Run("1 empty + 2 archives fires when a non-winner poller corroborates the head", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		poller := common.NewFakeEvmStatePoller(head, head)
		internal := common.NewFakeUpstream("internal-1", common.WithTags("type:internal"), common.WithEvmStatePoller(poller))
		ext1 := common.NewFakeUpstream("external-1", common.WithTags("type:external"), common.WithEvmStatePoller(poller))
		ext2 := common.NewFakeUpstream("external-2", common.WithTags("type:external"), common.WithEvmStatePoller(poller))

		// Network reports 0 — the served tip is unknown.
		winner := winnerWithHead(t, cfg, method, oldBlock, 0, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.Nil(t, winner.Error,
			"participant pollers must supply the head when the network tip is unknown, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	// Guard (issue 1363): two colluding archives inflate their poller latest
	// to 50M while the empty internal has no poller. Their MIN is still 50M,
	// so without a non-winner source a recent tx would look 30M deep.
	t.Run("head reported only by the winning group fails closed", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		inflated := common.NewFakeEvmStatePoller(50_000_000, 50_000_000)
		internal := common.NewFakeUpstream("internal-1", common.WithTags("type:internal"))
		ext1 := common.NewFakeUpstream("external-1", common.WithTags("type:external"), common.WithEvmStatePoller(inflated))
		ext2 := common.NewFakeUpstream("external-2", common.WithTags("type:external"), common.WithEvmStatePoller(inflated))

		winner := winnerOnNetwork(t, cfg, method, recentBlock, &headNetwork{latest: 50_000_000, maxMode: true}, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "a head only the winners report must not prove outside retention")
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire on winner-only head evidence")
	})

	// Guard: a single archive inflating its poller latest must not raise the
	// fallback head. MIN of two corroborated pollers keeps the honest tip, so
	// a recent winner stays inside retention and the quota holds.
	t.Run("inflated poller cannot raise fallback head on a recent winner", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		honest := common.NewFakeEvmStatePoller(head, head)
		inflated := common.NewFakeEvmStatePoller(50_000_000, 50_000_000)
		internal := common.NewFakeUpstream("internal-1", common.WithTags("type:internal"))
		ext1 := common.NewFakeUpstream("external-1", common.WithTags("type:external"), common.WithEvmStatePoller(honest))
		ext2 := common.NewFakeUpstream("external-2", common.WithTags("type:external"), common.WithEvmStatePoller(inflated))

		winner := winnerWithHead(t, cfg, method, recentBlock, 0, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error,
			"min of corroborated pollers must keep the honest tip, so a recent winner stays inside retention")
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute, common.ErrCodeConsensusCompositionDispute),
			"inflated poller must not trigger the waiver, got: %v", winner.Error)
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire on a recent winner")
	})

	// Guard: one poller is not a chain head. Without a second corroborating
	// source the fallback returns 0 and the waiver holds — fail closed.
	t.Run("single poller source fails closed", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		poller := common.NewFakeEvmStatePoller(head, head)
		internal := common.NewFakeUpstream("internal-1", common.WithTags("type:internal"))
		ext1 := common.NewFakeUpstream("external-1", common.WithTags("type:external"), common.WithEvmStatePoller(poller))
		ext2 := common.NewFakeUpstream("external-2", common.WithTags("type:external")) // no poller

		winner := winnerWithHead(t, cfg, method, oldBlock, 0, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "a single uncorroborated poller must not supply the head")
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute, common.ErrCodeConsensusCompositionDispute),
			"uncorroborated poller must fail closed, got: %v", winner.Error)
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire without a corroborated head")
	})

	// Guard: default max-mode served tip is a plain MAX a single inflator can
	// raise. chainHead must not trust it — fall through to corroborated pollers,
	// which keep the honest 20M tip so a recent winner stays inside retention.
	t.Run("max-mode network tip is not trusted", func(t *testing.T) {
		cfg := newCfg()
		cfg.maxParticipants = 3
		before := waivedMetricValue()

		honest := common.NewFakeEvmStatePoller(head, head)
		internal := common.NewFakeUpstream("internal-1", common.WithTags("type:internal"))
		ext1 := common.NewFakeUpstream("external-1", common.WithTags("type:external"), common.WithEvmStatePoller(honest))
		ext2 := common.NewFakeUpstream("external-2", common.WithTags("type:external"), common.WithEvmStatePoller(honest))

		winner := winnerOnNetwork(t, cfg, method, recentBlock, &headNetwork{latest: 50_000_000, maxMode: true}, []*execResult{
			emptyResultFrom(t, internal, 0),
			objResultFrom(t, ext1, tx, 1),
			objResultFrom(t, ext2, tx, 2),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error,
			"max-mode served tip must not fabricate an outside-retention proof")
		assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusDispute, common.ErrCodeConsensusCompositionDispute),
			"max-mode tip must not trigger the waiver, got: %v", winner.Error)
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire from an uncorroborated max-mode tip")
	})

	// Smokebox flake: network served tip can be 0 while internals have
	// blockAvailability.lower.latestBlockMinus. Avail-bound proof must still
	// promote the archives for an ancient winner block.
	t.Run("2 empty + 2 archives succeeds via avail lower bound when head is 0", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()

		minus := int64(100)
		poller := common.NewFakeEvmStatePoller(head, head)
		ba := &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: &minus},
		}
		in1 := common.NewFakeUpstream("internal-1",
			common.WithTags("type:internal"),
			common.WithEvmStatePoller(poller),
			common.WithEvmBlockAvailability(ba),
		)
		in2 := common.NewFakeUpstream("internal-2",
			common.WithTags("type:internal"),
			common.WithEvmStatePoller(poller),
			common.WithEvmBlockAvailability(ba),
		)
		e1 := taggedUpstream("external-1", "type:external")
		e2 := taggedUpstream("external-2", "type:external")

		winner := winnerWithHead(t, cfg, method, oldBlock, 0, []*execResult{
			emptyResultFrom(t, in1, 0),
			emptyResultFrom(t, in2, 1),
			objResultFrom(t, e1, tx, 2),
			objResultFrom(t, e2, tx, 3),
		})

		require.NotNil(t, winner)
		require.Nil(t, winner.Error,
			"avail lower bound must prove outside retention when network head is unknown, got: %v", winner.Error)
		require.NotNil(t, winner.Result)
		assert.Equal(t, 1.0, waivedMetricValue()-before,
			"exactly one empty_outside_retention waiver must be recorded")
	})

	t.Run("avail lower bound holds recent block when head is 0", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()

		minus := int64(100)
		poller := common.NewFakeEvmStatePoller(head, head)
		ba := &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: &minus},
		}
		in1 := common.NewFakeUpstream("internal-1",
			common.WithTags("type:internal"),
			common.WithEvmStatePoller(poller),
			common.WithEvmBlockAvailability(ba),
		)
		in2 := common.NewFakeUpstream("internal-2",
			common.WithTags("type:internal"),
			common.WithEvmStatePoller(poller),
			common.WithEvmBlockAvailability(ba),
		)
		e1 := taggedUpstream("external-1", "type:external")
		e2 := taggedUpstream("external-2", "type:external")

		winner := winnerWithHead(t, cfg, method, recentBlock, 0, []*execResult{
			emptyResultFrom(t, in1, 0),
			emptyResultFrom(t, in2, 1),
			objResultFrom(t, e1, tx, 2),
			objResultFrom(t, e2, tx, 3),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "recent block inside avail lower bound must not waive")
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver for in-bound recent block")
	})

	t.Run("unbounded avail + head 0 fails closed", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()

		// Internals with no BlockAvailability → MinInt64 lower bound.
		in1 := taggedUpstream("internal-1", "type:internal")
		in2 := taggedUpstream("internal-2", "type:internal")
		e1 := taggedUpstream("external-1", "type:external")
		e2 := taggedUpstream("external-2", "type:external")

		winner := winnerWithHead(t, cfg, method, oldBlock, 0, []*execResult{
			emptyResultFrom(t, in1, 0),
			emptyResultFrom(t, in2, 1),
			objResultFrom(t, e1, tx, 2),
			objResultFrom(t, e2, tx, 3),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "no avail bound and no head must fail closed")
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver without proof")
	})

	// Guard: without opt-in, the balanced tie behaves exactly as before — a
	// plain dispute — so the rescue cannot alter existing configs.
	t.Run("balanced tie stays a dispute without opt-in", func(t *testing.T) {
		cfg := newCfg()
		cfg.requiredParticipants = []*common.ConsensusRequiredParticipant{
			{Tag: "type:internal", MinParticipants: 1, MinAgreement: 1}, // no waiver opt-in
			{Tag: "type:external", MinParticipants: 1, MinAgreement: 1},
		}

		winner := winnerWithHead(t, cfg, method, oldBlock, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			emptyResultFrom(t, internal2, 1),
			objResultFrom(t, ext1, tx, 2),
			objResultFrom(t, ext2, tx, 3),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "without opt-in the tie must remain unresolved")
	})

	// Guard (issue 1363): a hash lookup names no block. Two agreeing archives
	// claim the tx sits in ancient block 16; that claim is theirs, so it must
	// not prove the empty internal pruned it. The round stays a dispute.
	t.Run("hash lookup with fabricated old block in answers fails closed", func(t *testing.T) {
		cfg := newCfg()
		before := waivedMetricValue()
		fake := map[string]interface{}{"blockNumber": "0x10"}

		winner := winnerWithHead(t, cfg, "eth_getTransactionByHash", 0, head, []*execResult{
			emptyResultFrom(t, internal1, 0),
			emptyResultFrom(t, internal2, 1),
			objResultFrom(t, ext1, fake, 2),
			objResultFrom(t, ext2, fake, 3),
		})

		require.NotNil(t, winner)
		require.NotNil(t, winner.Error, "answers must never prove their own block is outside retention")
		assert.Equal(t, 0.0, waivedMetricValue()-before, "no waiver may fire without a requested block")
	})
}

func missingDataWaivedMetricValue() float64 {
	return testutil.ToFloat64(
		telemetry.MetricConsensusCompositionWaived.WithLabelValues(
			"test-project", "evm:1", "type:internal", waiverReasonMissingData),
	)
}

// TestMissingDataWaiver_StgEthGetBalanceE2E is the determineWinner + metric
// proof for the stg eth_getBalance miss: PreferNonEmpty+ReturnError (no
// AcceptMostCommon), both waiver flags on internal, a request naming no block,
// wrapped ErrUpstreamRequest/ErrEndpointMissingData from both internals, two
// agreeing externals. Must return the external value and increment
// erpc_consensus_composition_waived_total{reason=missing_data}.
func TestMissingDataWaiver_StgEthGetBalanceE2E(t *testing.T) {
	preferNE := true
	preferLR := false
	cfg := &config{
		maxParticipants:         4,
		agreementThreshold:      2,
		preferNonEmpty:          preferNE,
		preferLargerResponses:   preferLR,
		disputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		requiredParticipants: []*common.ConsensusRequiredParticipant{
			{
				Tag: "type:internal", MinParticipants: 2, MinAgreement: 1,
				WaiveAgreementOnMissingData:           true,
				WaiveAgreementOnEmptyOutsideRetention: true,
				RetentionBlocks:                       2350000,
			},
			{Tag: "type:external", MinParticipants: 2, MinAgreement: 1},
		},
	}
	int1 := taggedUpstream("internal-blue", "type:internal")
	int2 := taggedUpstream("internal-green", "type:internal")
	ext1 := taggedUpstream("alchemy", "type:external")
	ext2 := taggedUpstream("quicknode", "type:external")
	inner := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorMissingData, "historical state is not available", nil, nil),
		nil,
	)
	wrap := func(ups common.Upstream) error {
		return common.NewErrUpstreamRequest(inner, ups, "evm:1", "eth_getBalance", time.Millisecond, 1, 0, 0)
	}

	before := missingDataWaivedMetricValue()
	winner := winnerWithHead(t, cfg, "eth_getBalance", 0, 26_090_571, []*execResult{
		errorFrom(int1, wrap(int1), 0),
		errorFrom(int2, wrap(int2), 1),
		resultFrom(t, ext1, "0x1234", 2),
		resultFrom(t, ext2, "0x1234", 3),
	})
	require.NotNil(t, winner)
	require.Nil(t, winner.Error, "MissingData waiver must release the internal quota, got: %v", winner.Error)
	require.NotNil(t, winner.Result)
	assert.Equal(t, 1.0, missingDataWaivedMetricValue()-before,
		"erpc_consensus_composition_waived_total{reason=missing_data} must increment")
}
