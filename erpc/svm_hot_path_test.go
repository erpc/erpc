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

// Every upstream's floor is probed, but without a declared firstAvailableSlotPlus
// it is never enforced, so every slot-named read — point or range — reaches the upstream.
func TestSvm_ObservedFloor_NeverSkipsWithoutDeclaredBound(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	util.SetupMocksForSvmStatePollerWithFirstAvailable("v1.localhost", 600000000, 599999900, 600000001, 509000000)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("v2.localhost", 600000000, 599999900, 600000001, 550000000)

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
		_, known := ups[i].SvmStatePoller().FirstAvailableSlot()
		require.Truef(t, known, "%s floor is probed even without a declared bound", ups[i].Id())
	}

	for _, body := range []struct{ method, req string }{
		{"getBlock", `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[300000000]}`},
		{"getBlockTime", `{"jsonrpc":"2.0","id":1,"method":"getBlockTime","params":[300000000]}`},
		{"getBlocks", `{"jsonrpc":"2.0","id":1,"method":"getBlocks","params":[300000000,300000010]}`},
		{"getBlocksWithLimit", `{"jsonrpc":"2.0","id":1,"method":"getBlocksWithLimit","params":[300000000,10]}`},
	} {
		r := common.NewNormalizedRequest([]byte(body.req))
		r.SetUpstreams(selected)
		for _, u := range ups {
			skipErr, _ := network.checkUpstreamBlockAvailability(ctx, u, r, body.method)
			require.NoErrorf(t, skipErr, "%s %s must not be skipped on a vendor-reported floor", u.Id(), body.method)
		}
	}
}

// firstAvailableSlotPlus is what enforces the floor: the operator anchors a declared window
// on the observed floor, and only that upstream is skipped below it. An
// upstream declaring exactSlot: 0 overrides the vendor's higher reported floor.
func TestSvm_FirstAvailableSlotPlus_OptInUsesObservedFloor(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	util.SetupMocksForSvmStatePollerWithFirstAvailable("validator.localhost", 300000, 299900, 300001, 200000)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("archive.localhost", 300000, 299900, 300001, 200000)

	plus := int64(0)
	zero := int64(0)
	validator := svmUpstreamCfg("validator", "validator.localhost")
	validator.Svm.BlockAvailability = &common.SvmBlockAvailabilityConfig{
		Lower: &common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: &plus},
	}
	archive := svmUpstreamCfg("archive", "archive.localhost")
	archive.Svm.BlockAvailability = &common.SvmBlockAvailabilityConfig{
		Lower: &common.SvmAvailabilityBoundConfig{ExactSlot: &zero},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, upsReg := setupTestSvmNetworkFinalized(t, ctx, []*common.UpstreamConfig{validator, archive})
	time.Sleep(300 * time.Millisecond)

	ups := upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta"))
	require.Len(t, ups, 2)
	byId := map[string]common.Upstream{}
	for _, u := range ups {
		byId[u.Id()] = u
	}

	floor, known := byId["validator"].(common.SvmUpstream).SvmStatePoller().FirstAvailableSlot()
	require.True(t, known, "the declaring upstream's floor is observed")
	require.EqualValues(t, 200000, floor)
	floor, known = byId["archive"].(common.SvmUpstream).SvmStatePoller().FirstAvailableSlot()
	require.True(t, known, "every upstream's floor is probed")
	require.EqualValues(t, 200000, floor, "the archive's vendor-reported floor is known but overridden by exactSlot")

	below := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[1000]}`))
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, byId["validator"], below, "getBlock")
	require.Error(t, skipErr, "firstAvailableSlotPlus anchors the lower bound on the observed floor")
	require.False(t, retryable)
	require.True(t, common.IsPermanentlyMissingData(skipErr))

	archiveErr, _ := network.checkUpstreamBlockAvailability(ctx, byId["archive"], below, "getBlock")
	require.NoError(t, archiveErr, "exactSlot: 0 overrides the vendor's reported floor")

	above := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[250000]}`))
	for id, u := range byId {
		err, _ := network.checkUpstreamBlockAvailability(ctx, u, above, "getBlock")
		require.NoErrorf(t, err, "%s must serve a slot above the floor", id)
	}
}
