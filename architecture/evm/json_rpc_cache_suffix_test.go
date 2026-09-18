package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type suffixNet struct {
	id     string
	suffix string
}

func (n suffixNet) Id() string                               { return n.id }
func (n suffixNet) Label() string                            { return n.id }
func (n suffixNet) ProjectId() string                        { return "test" }
func (n suffixNet) Architecture() common.NetworkArchitecture { return common.ArchitectureEvm }
func (n suffixNet) Config() *common.NetworkConfig {
	return &common.NetworkConfig{CacheKeySuffix: n.suffix}
}
func (n suffixNet) Logger() *zerolog.Logger                       { l := zerolog.Nop(); return &l }
func (n suffixNet) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (n suffixNet) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (n suffixNet) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}

func blockByNumberReq(id, suffix string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3d5c04a",false]}`))
	req.SetNetwork(suffixNet{id: id, suffix: suffix})
	return req
}

func TestGenerateKeysForJsonRpcRequest_CacheKeySuffix(t *testing.T) {
	t.Parallel()

	empty, rkEmpty, err := generateKeysForJsonRpcRequest(blockByNumberReq("evm:998", ""), "64321354")
	require.NoError(t, err)
	require.Equal(t, "evm:998:64321354", empty, "empty suffix must keep today's key")

	suffixed, rkSuffixed, err := generateKeysForJsonRpcRequest(blockByNumberReq("evm:998", "systx"), "64321354")
	require.NoError(t, err)
	require.Equal(t, "evm:998:systx:64321354", suffixed)

	require.Equal(t, rkEmpty, rkSuffixed, "range key is method+params; suffix only affects the partition")

	other, _, err := generateKeysForJsonRpcRequest(blockByNumberReq("evm:998", "archive"), "64321354")
	require.NoError(t, err)
	require.NotEqual(t, suffixed, other)
	require.NotEqual(t, empty, other)

	nilRef, _, err := generateKeysForJsonRpcRequest(blockByNumberReq("evm:998", "systx"), "")
	require.NoError(t, err)
	require.Equal(t, "evm:998:systx:nil", nilRef)
}
