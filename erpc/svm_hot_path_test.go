package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func svmUpstreamCfg(id, host string) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeSvm,
		Endpoint: "http://" + host,
		Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
	}
}

// Range reads (getBlocks/getBlocksWithLimit) are a routing PREFERENCE, not a
// refusal: agave answers a start below its floor by clamping to the lowest
// slot it retains, so when NO upstream in the pool covers the start, the
// request must still forward everywhere — refusing it would return a -32001
// agave itself never emits for these methods, breaking history discovery
// (getBlocksWithLimit(0, N) is how clients find where history begins).
func TestSvm_LedgerFloor_RangeForwardedWhenNoUpstreamCovers(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	// Both upstreams pruned well above the named start: nothing covers it.
	util.SetupMocksForSvmStatePollerWithFirstAvailable("v1.localhost", 300000, 299900, 300001, 200000)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("v2.localhost", 300000, 299900, 300001, 250000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, upsReg := setupTestSvmNetworkFinalized(t, ctx, []*common.UpstreamConfig{
		svmUpstreamCfg("v1", "v1.localhost"),
		svmUpstreamCfg("v2", "v2.localhost"),
	})
	time.Sleep(300 * time.Millisecond)

	ups := upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta"))
	require.Len(t, ups, 2)

	selected := make([]common.Upstream, len(ups))
	for i := range ups {
		selected[i] = ups[i]
	}

	// The range read forwards to both, floors notwithstanding — agave clamps.
	belowAll := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlocks","params":[1000,1010]}`))
	belowAll.SetUpstreams(selected)
	for _, u := range ups {
		skipErr, _ := network.checkUpstreamBlockAvailability(ctx, u, belowAll, "getBlocks")
		require.NoErrorf(t, skipErr, "%s must not be skipped: no upstream covers the start, so agave clamps", u.Id())
	}

	// Point lookups stay strict: still skipped below every known floor, so the
	// sweep reports permanent missing data instead of burning a round trip.
	point := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[1000]}`))
	point.SetUpstreams(selected)
	for _, u := range ups {
		skipErr, _ := network.checkUpstreamBlockAvailability(ctx, u, point, "getBlock")
		require.Errorf(t, skipErr, "%s must still skip a point lookup below its floor", u.Id())
	}
}

// The ledger floor must keep a below-floor read off the validator entirely.
// Below its floor the validator answers getBlocks with a correct but silently
// short list of local roots, which the empty-result sweep accepts as complete.
func TestSvm_LedgerFloor_SkipsUpstreamBelowItsFloor(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	util.SetupMocksForSvmStatePollerWithFirstAvailable("validator.localhost", 300000, 299900, 300001, 200000)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("archive.localhost", 300000, 299900, 300001, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, upsReg := setupTestSvmNetworkFinalized(t, ctx, []*common.UpstreamConfig{
		svmUpstreamCfg("validator", "validator.localhost"),
		svmUpstreamCfg("archive", "archive.localhost"),
	})
	time.Sleep(300 * time.Millisecond)

	ups := upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta"))
	require.Len(t, ups, 2)

	byId := map[string]common.Upstream{}
	selected := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		byId[u.Id()] = u
		selected = append(selected, u)
	}

	// A slot below the validator's floor: skipped there, servable on the archive.
	belowFloor := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlocks","params":[1000,1010]}`))
	belowFloor.SetUpstreams(selected)
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, byId["validator"], belowFloor, "getBlocks")
	require.Error(t, skipErr, "the validator cannot serve a slot below its ledger floor")
	require.False(t, retryable, "a floor only rises, so waiting cannot help")
	require.True(t, common.IsPermanentlyMissingData(skipErr))

	archiveErr, _ := network.checkUpstreamBlockAvailability(ctx, byId["archive"], belowFloor, "getBlocks")
	require.NoError(t, archiveErr, "the archive covers genesis, so it must not be skipped")

	// A slot above the validator's floor is servable on both.
	aboveFloor := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlocks","params":[250000,250010]}`))
	aboveFloor.SetUpstreams(selected)
	for id, u := range byId {
		err, _ := network.checkUpstreamBlockAvailability(ctx, u, aboveFloor, "getBlocks")
		require.NoErrorf(t, err, "%s must serve a slot above every floor", id)
	}
}
