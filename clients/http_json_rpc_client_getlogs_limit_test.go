package clients

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

type testNetworkGetLogsLimit struct {
	cfg *common.NetworkConfig
}

var _ common.Network = (*testNetworkGetLogsLimit)(nil)

func (n *testNetworkGetLogsLimit) Id() string { return "evm:8453" }
func (n *testNetworkGetLogsLimit) Label() string {
	return "evm:8453"
}
func (n *testNetworkGetLogsLimit) ProjectId() string { return "test" }
func (n *testNetworkGetLogsLimit) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *testNetworkGetLogsLimit) Config() *common.NetworkConfig { return n.cfg }
func (n *testNetworkGetLogsLimit) Logger() *zerolog.Logger       { return &log.Logger }
func (n *testNetworkGetLogsLimit) GetMethodMetrics(method string) common.TrackedMetrics {
	return nil
}
func (n *testNetworkGetLogsLimit) Forward(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	panic("not used")
}
func (n *testNetworkGetLogsLimit) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (n *testNetworkGetLogsLimit) Cache() common.CacheDAL { return nil }
func (n *testNetworkGetLogsLimit) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return 0
}
func (n *testNetworkGetLogsLimit) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return 0
}
func (n *testNetworkGetLogsLimit) EvmLeaderUpstream(ctx context.Context) common.Upstream { return nil }

func TestHttpJsonRpcClient_EthGetLogs_ResponseSizeCap_ReturnsTooLarge(t *testing.T) {
	util.ConfigureTestLogger()

	// Response bigger than cap; ensure we error out with ErrEndpointRequestTooLarge (to enable splitting).
	capBytes := int64(1024)

	// Build a deterministic large JSON-RPC response.
	// result is an array of strings to mimic logs payload shape.
	payload := `{"jsonrpc":"2.0","id":1,"result":[` + strings.Repeat(`"x",`, 3000) + `"x"]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, payload)
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	logger := log.Logger
	ups := common.NewFakeUpstream("rpc1")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = srv.URL

	client, err := NewGenericHttpJsonRpcClient(context.Background(), &logger, "prj1", ups, parsed, ups.Config().JsonRpc, nil, &noopErrorExtractor{})
	require.NoError(t, err)

	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x2",
			"topics":    []interface{}{"0xabc"},
		},
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(&testNetworkGetLogsLimit{
		cfg: &common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{
				GetLogsMaxResponseBytes: capBytes,
				GetLogsSplitOnError:     util.BoolPtr(true),
			},
		},
	})

	_, err = client.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge), "expected ErrEndpointRequestTooLarge, got: %v", err)
}

func TestHttpJsonRpcClient_EthGetLogs_ContentLengthTooLarge_FastFail(t *testing.T) {
	util.ConfigureTestLogger()

	capBytes := int64(1024)

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "9999999")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// If client doesn't fast-fail on Content-Length, it'll block trying to read the body.
		<-r.Context().Done()
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	logger := log.Logger
	ups := common.NewFakeUpstream("rpc1")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = srv.URL

	client, err := NewGenericHttpJsonRpcClient(context.Background(), &logger, "prj1", ups, parsed, ups.Config().JsonRpc, nil, &noopErrorExtractor{})
	require.NoError(t, err)

	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x2",
			"topics":    []interface{}{"0xabc"},
		},
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(&testNetworkGetLogsLimit{
		cfg: &common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{
				GetLogsMaxResponseBytes: capBytes,
				GetLogsSplitOnError:     util.BoolPtr(true),
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err = client.SendRequest(ctx, req)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge), "expected ErrEndpointRequestTooLarge, got: %v", err)
	require.Less(t, time.Since(start), 250*time.Millisecond)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("server handler did not observe client cancellation")
	}
}
