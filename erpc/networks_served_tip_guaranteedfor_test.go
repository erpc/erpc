package erpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

func init() { util.ConfigureTestLogger() }

// These tests pin servedTip.guaranteedFor: the advertised tip is clamped down
// to the OWN MAJORITY of every operator-named upstream group, so a mixed pool
// never advertises a block the group consensus requires cannot serve.
//
// Every fixture is built so the network-wide majority differs from the group's,
// which is what makes a regression visible.

const (
	tagInternal = "type:internal"
	tagExternal = "type:external"
)

// mixedPool builds a tagged pool: externals first, then internals.
func mixedPool(externalHeads, internalHeads []int64) []servedTipFixture {
	out := make([]servedTipFixture, 0, len(externalHeads)+len(internalHeads))
	for i, h := range externalHeads {
		out = append(out, servedTipFixture{
			id: fmt.Sprintf("ext%d", i+1), chainID: 123, latestBlock: h, tags: []string{tagExternal},
		})
	}
	for i, h := range internalHeads {
		out = append(out, servedTipFixture{
			id: fmt.Sprintf("int%d", i+1), chainID: 123, latestBlock: h, tags: []string{tagInternal},
		})
	}
	return out
}

func guaranteedForNetwork(t *testing.T, ctx context.Context, fx []servedTipFixture, cfg *common.EvmServedTipConfig) *Network {
	t.Helper()
	util.SetupMocksForEvmStatePoller()
	n, _ := setupServedTipNetworkWith(t, ctx, fx, cfg)
	return n
}

// Unset is today's behaviour, exactly: the network-wide majority stands.
func TestGuaranteedFor_UnsetIsNoOp(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}})
	assert.Equal(t, int64(1000), n.EvmHighestLatestBlockNumber(ctx))
}

// The advertised tip is one the named group can actually serve.
func TestGuaranteedFor_ClampsToTheNamedGroupsMajority(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{tagInternal}})
	assert.Equal(t, int64(993), n.EvmHighestLatestBlockNumber(ctx),
		"network majority is 1000, but the internals only have 993")
}

// The clamp tracks the group's ACTUAL lag — no configured constant to keep in
// step with the fleet.
func TestGuaranteedFor_TracksTheActualLag(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, tc := range []struct {
		name     string
		internal []int64
		want     int64
	}{
		{"two blocks behind", []int64{998, 998}, 998},
		{"forty blocks behind", []int64{960, 960}, 960},
		{"caught up costs nothing", []int64{1000, 1000}, 1000},
		{"ahead of the network majority never raises the tip", []int64{1002, 1002}, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, tc.internal),
				&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{tagInternal}})
			assert.Equal(t, tc.want, n.EvmHighestLatestBlockNumber(ctx))
		})
	}
}

// The clamp is a group MAJORITY, not a group minimum: one frozen member is
// outvoted inside its own group and cannot pin the network.
func TestGuaranteedFor_StuckMemberDoesNotPinTheNetwork(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993, 400}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{tagInternal}})
	assert.Equal(t, int64(993), n.EvmHighestLatestBlockNumber(ctx),
		"int3 frozen at 400 is outvoted by its own group")
}

// A one-member group has no majority to outvote a stall, so that upstream does
// set the tip. Same property guaranteedMethods already has for a method only
// one upstream supports — naming a single upstream IS declaring it mandatory.
func TestGuaranteedFor_SingleMemberGroupSetsTheTip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{400}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{tagInternal}})
	assert.Equal(t, int64(400), n.EvmHighestLatestBlockNumber(ctx))
}

// A selector matching nothing constrains nothing — it never pins the tip to 0.
func TestGuaranteedFor_UnmatchedGroupConstrainsNothing(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{"type:nonexistent"}})
	assert.Equal(t, int64(1000), n.EvmHighestLatestBlockNumber(ctx))
}

// Several groups fold to the LOWEST of their majorities — the tip must be
// servable by every named group, not just one.
func TestGuaranteedFor_MultipleGroupsTakeTheLowest(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fx := []servedTipFixture{
		{id: "ext1", chainID: 123, latestBlock: 1000, tags: []string{tagExternal}},
		{id: "ext2", chainID: 123, latestBlock: 1000, tags: []string{tagExternal}},
		{id: "ext3", chainID: 123, latestBlock: 1000, tags: []string{tagExternal}},
		{id: "int1", chainID: 123, latestBlock: 993, tags: []string{tagInternal}},
		{id: "int2", chainID: 123, latestBlock: 993, tags: []string{tagInternal}},
		{id: "arch1", chainID: 123, latestBlock: 980, tags: []string{"role:archive"}},
		{id: "arch2", chainID: 123, latestBlock: 980, tags: []string{"role:archive"}},
	}
	n := guaranteedForNetwork(t, ctx, fx, &common.EvmServedTipConfig{
		EnabledFor:    []string{"latest"},
		GuaranteedFor: []string{tagInternal, "role:archive"},
	})
	assert.Equal(t, int64(980), n.EvmHighestLatestBlockNumber(ctx),
		"lowest group majority wins: archives at 980 below internals at 993")
}

// An id glob selects too — guaranteedFor takes the same selector vocabulary as
// use-upstream and consensus.requiredParticipants.
func TestGuaranteedFor_MatchesByUpstreamIdGlob(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := guaranteedForNetwork(t, ctx, mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993}),
		&common.EvmServedTipConfig{EnabledFor: []string{"latest"}, GuaranteedFor: []string{"int*"}})
	assert.Equal(t, int64(993), n.EvmHighestLatestBlockNumber(ctx))
}

// The guarantee applies to the finalized axis on its own terms: the finalized
// tip is clamped to the group's finalized majority, not to its latest one.
func TestGuaranteedFor_AppliesToTheFinalizedAxis(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	util.SetupMocksForEvmStatePoller()
	fx := mixedPool([]int64{1000, 1000, 1000}, []int64{993, 993})
	network, ups := setupServedTipNetworkWith(t, ctx, fx, &common.EvmServedTipConfig{
		EnabledFor:    []string{"finalized"},
		GuaranteedFor: []string{tagInternal},
	})

	finalized := []int64{900, 900, 900, 880, 880} // externals first, then internals
	for i, u := range ups {
		setServedTipFinalized(t, u, finalized[i])
	}

	assert.Equal(t, int64(880), network.EvmHighestFinalizedBlockNumber(ctx),
		"finalized majority is 900; the internals have only finalized 880")
}
