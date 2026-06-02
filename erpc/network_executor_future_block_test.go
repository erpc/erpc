package erpc

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fbFakeNetwork is a minimal common.Network used to exercise the future-block
// retry gate in shouldRetryWithReason without standing up a real Network.
type fbFakeNetwork struct {
	cfg  *common.NetworkConfig
	head int64
}

func (n *fbFakeNetwork) Id() string        { return "test" }
func (n *fbFakeNetwork) Label() string     { return "test" }
func (n *fbFakeNetwork) ProjectId() string { return "test" }
func (n *fbFakeNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *fbFakeNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *fbFakeNetwork) Logger() *zerolog.Logger       { l := zerolog.Nop(); return &l }
func (n *fbFakeNetwork) GetMethodMetrics(method string) common.TrackedMetrics {
	return nil
}
func (n *fbFakeNetwork) Forward(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (n *fbFakeNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *fbFakeNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64    { return n.head }
func (n *fbFakeNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 { return 0 }
func (n *fbFakeNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream    { return nil }

func fbNetworkConfig(distance *int64, head int64) *fbFakeNetwork {
	return &fbFakeNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				MarkEmptyAsErrorMethods:     common.DefaultMarkEmptyAsErrorMethods(),
				MaxFutureBlockRetryDistance: distance,
			},
		},
		head: head,
	}
}

func fbExecutorRequest(t *testing.T, net common.Network, blockNumber int64) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1"]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	req.SetNetwork(net)
	req.SetEvmBlockNumber(blockNumber)
	return req
}

func fbEmptyishResponse(t *testing.T, req *common.NormalizedRequest) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func fbNewExecutor(t *testing.T) *networkExecutor {
	t.Helper()
	lg := zerolog.Nop()
	e, err := NewNetworkExecutor(nil, &lg, nil, nil)
	require.NoError(t, err)
	return e
}

func i64ptr(v int64) *int64 { return &v }

func TestShouldRetry_EmptyResult_FutureBlockNotRetried(t *testing.T) {
	e := fbNewExecutor(t)
	net := fbNetworkConfig(i64ptr(1), 1000)

	t.Run("future block: empty is accepted, not retried", func(t *testing.T) {
		req := fbExecutorRequest(t, net, 2000)
		resp := fbEmptyishResponse(t, req)
		assert.Equal(t, "", e.shouldRetryWithReason(req, resp, nil, 0))
	})

	t.Run("at/below head: empty is retried as today", func(t *testing.T) {
		req := fbExecutorRequest(t, net, 999)
		resp := fbEmptyishResponse(t, req)
		assert.Equal(t, "empty_result", e.shouldRetryWithReason(req, resp, nil, 0))
	})
}

func TestShouldRetry_MissingData_FutureBlockNotRetried(t *testing.T) {
	e := fbNewExecutor(t)
	net := fbNetworkConfig(i64ptr(1), 1000)
	missing := common.NewErrEndpointMissingData(errors.New("empty"), nil)

	t.Run("future block: missing-data is not retried", func(t *testing.T) {
		req := fbExecutorRequest(t, net, 2000)
		assert.Equal(t, "", e.shouldRetryWithReason(req, nil, missing, 0))
	})

	t.Run("at/below head: missing-data is retried as today", func(t *testing.T) {
		req := fbExecutorRequest(t, net, 999)
		assert.Equal(t, "missing_data", e.shouldRetryWithReason(req, nil, missing, 0))
	})
}

func TestShouldRetry_FutureBlock_DefaultOn_NotRetried(t *testing.T) {
	e := fbNewExecutor(t)
	net := fbNetworkConfig(nil, 1000) // unset -> default bound of 1 is active
	missing := common.NewErrEndpointMissingData(errors.New("empty"), nil)

	req := fbExecutorRequest(t, net, 5000)
	resp := fbEmptyishResponse(t, req)

	assert.Equal(t, "", e.shouldRetryWithReason(req, resp, nil, 0))
	assert.Equal(t, "", e.shouldRetryWithReason(req, nil, missing, 0))
}

func TestShouldRetry_FutureBlock_LargeBoundOptsOut_RetriesAsBaseline(t *testing.T) {
	e := fbNewExecutor(t)
	net := fbNetworkConfig(i64ptr(1_000_000), 1000) // very large bound effectively opts out
	missing := common.NewErrEndpointMissingData(errors.New("empty"), nil)

	req := fbExecutorRequest(t, net, 5000)
	resp := fbEmptyishResponse(t, req)

	assert.Equal(t, "empty_result", e.shouldRetryWithReason(req, resp, nil, 0))
	assert.Equal(t, "missing_data", e.shouldRetryWithReason(req, nil, missing, 0))
}

func TestShouldRetry_FutureBlock_ColdHead_RetriesAsBaseline(t *testing.T) {
	e := fbNewExecutor(t)
	net := fbNetworkConfig(i64ptr(1), 0) // head unknown -> fail open

	req := fbExecutorRequest(t, net, 5000)
	resp := fbEmptyishResponse(t, req)

	assert.Equal(t, "empty_result", e.shouldRetryWithReason(req, resp, nil, 0))
}
