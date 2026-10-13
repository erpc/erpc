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

func TestSvm_DeclaredBounds_SkipsBelowConfiguredLower(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	// latestSlotMinus: 100000 is an operator restriction on the window.
	util.SetupMocksForSvmStatePollerWithFirstAvailable("archive.localhost", 300000, 299900, 300001, 0)

	minus := int64(100000)
	cfg := svmUpstreamCfg("archive", "archive.localhost")
	cfg.Svm.BlockAvailability = &common.SvmBlockAvailabilityConfig{
		Lower: &common.SvmAvailabilityBoundConfig{LatestSlotMinus: &minus},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, upsReg := setupTestSvmNetworkFinalized(t, ctx, []*common.UpstreamConfig{cfg})
	time.Sleep(300 * time.Millisecond)

	ups := upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta"))
	require.Len(t, ups, 1)

	below := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[1000]}`))
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, ups[0], below, "getBlock")
	require.Error(t, skipErr, "declared lower is latest-100000; slot 1000 is below it")
	require.False(t, retryable, "a declared lower is permanent for this request")
	require.True(t, common.IsPermanentlyMissingData(skipErr))

	inside := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[250000]}`))
	okErr, _ := network.checkUpstreamBlockAvailability(ctx, ups[0], inside, "getBlock")
	require.NoError(t, okErr)
}

func TestSvm_DeclaredBounds_DisabledWhenEnforceBlockAvailabilityFalse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	util.SetupMocksForSvmStatePollerWithFirstAvailable("archive.localhost", 300000, 299900, 300001, 0)

	minus := int64(100000)
	cfg := svmUpstreamCfg("archive", "archive.localhost")
	cfg.Svm.BlockAvailability = &common.SvmBlockAvailabilityConfig{
		Lower: &common.SvmAvailabilityBoundConfig{LatestSlotMinus: &minus},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, upsReg := setupTestSvmNetworkFinalized(t, ctx, []*common.UpstreamConfig{cfg})
	network.cfg.Svm.EnforceBlockAvailability = util.BoolPtr(false)
	time.Sleep(300 * time.Millisecond)

	ups := upsReg.GetNetworkUpstreams(ctx, util.SvmNetworkId("", "mainnet-beta"))
	require.Len(t, ups, 1)

	below := common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[1000]}`))
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, ups[0], below, "getBlock")
	require.NoError(t, skipErr, "enforceBlockAvailability:false must disable declared bounds too")
	require.False(t, retryable)
}
