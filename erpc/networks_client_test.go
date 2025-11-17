package erpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetwork_TraceExecutionTimeout(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx, nil, nil)

	// Mock trace timeout shape returned in result body
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return strings.Contains(body, "debug_traceBlockByNumber")
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":[{"error":"execution timeout"}]}`)

	time.Sleep(100 * time.Millisecond)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"debug_traceBlockByNumber","params":["0x226AE",{"tracer":"callTracer","timeout":"1ms"}],"id":1}`))
	resp, err := network.Forward(ctx, req)

	require.Error(t, err)
	assert.Nil(t, resp)

	// Verify normalized timeout code (-32015) via extractor output in the error chain
	var jre *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(err, &jre), "expected ErrJsonRpcExceptionInternal in error chain")
	assert.Equal(t, common.JsonRpcErrorNodeTimeout, jre.NormalizedCode())
	assert.Contains(t, err.Error(), "execution timeout")
}

func TestNetwork_CapacityExceededErrors(t *testing.T) {
	t.Run("Direct429Response", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkSimple(t, ctx, nil, nil)

		// Set up a 429 response with an empty body for the actual request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_blockNumber")
			}).
			Reply(429).
			BodyString("")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		resp, err := network.Forward(ctx, req)

		require.Error(t, err)
		assert.Nil(t, resp)

		// Expect ErrEndpointCapacityExceeded somewhere in the chain
		var capErr *common.ErrEndpointCapacityExceeded
		require.True(t, errors.As(err, &capErr), "Expected ErrEndpointCapacityExceeded, got: %T", err)

		// Verify the normalized code and preserved status code from extractor
		var jre *common.ErrJsonRpcExceptionInternal
		require.True(t, errors.As(err, &jre), "Expected ErrJsonRpcExceptionInternal in error chain")
		assert.Equal(t, common.JsonRpcErrorCapacityExceeded, jre.NormalizedCode())
		assert.Equal(t, 429, jre.Details["statusCode"])
	})

	t.Run("429WithErrorMessage", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkSimple(t, ctx, nil, nil)

		// Set up a 429 response with a rate limit error message
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_blockNumber")
			}).
			Reply(429).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Rate limit exceeded"}}`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err := network.Forward(ctx, req)

		// Verify the error
		assert.Error(t, err)
		// Check that we got a capacity exceeded error
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "Expected ErrEndpointCapacityExceeded, got: %T: %v", err, err)
		// Verify the error message was preserved
		assert.Contains(t, err.Error(), "Rate limit exceeded")
	})
}

func TestNetwork_BatchRequests(t *testing.T) {
	t.Run("SimpleBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upCfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
				BatchMaxSize:  5,
				BatchMaxWait:  common.Duration(50 * time.Millisecond),
			},
		}
		network := setupTestNetworkSimple(t, ctx, upCfg, nil)
		// Ensure upstreams are prepared for the network id
		err := network.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		// Mock a batch response with three results
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_blockNumber")
			}).
			Persist().
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`)

		// Fire 3 concurrent requests that will be batched by the client layer
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				resp, err := network.Forward(ctx, req)
				if err != nil {
					assert.NotContains(t, err.Error(), "no response received for request")
				}
				assert.NoError(t, err)
				// Ensure we got a valid single-object JSON-RPC response
				var wr bytes.Buffer
				_, werr := resp.WriteTo(&wr)
				assert.NoError(t, werr)
				txt := wr.String()
				// Should match one of the expected result objects
				assert.Contains(t, txt, `"jsonrpc":"2.0"`)
				assert.Contains(t, txt, `"result"`)
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SeparateBatchRequestsWithSameIDs", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upCfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
				BatchMaxSize:  5,
				BatchMaxWait:  common.Duration(500 * time.Millisecond),
			},
		}
		network := setupTestNetworkSimple(t, ctx, upCfg, nil)
		err := network.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		// Allow async upstream bootstrapping to settle before issuing batch requests
		time.Sleep(100 * time.Millisecond)

		// Expect multiple batch calls, each answering for id=1 and id=6
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_blockNumber")
			}).
			Persist().
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":6,"result":"0x6"}]`)

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
				resp1, err1 := network.Forward(ctx, req1)
				assert.NoError(t, err1)
				require.NotNil(t, resp1)
				var wr1 bytes.Buffer
				_, er1 := resp1.WriteTo(&wr1)
				assert.NoError(t, er1)
				txt1 := wr1.String()
				assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, txt1)
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				req6 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":6,"method":"eth_blockNumber","params":[]}`))
				resp6, err6 := network.Forward(ctx, req6)
				assert.NoError(t, err6)
				require.NotNil(t, resp6)
				var wr6 bytes.Buffer
				_, er6 := resp6.WriteTo(&wr6)
				assert.NoError(t, er6)
				txt6 := wr6.String()
				assert.Equal(t, `{"jsonrpc":"2.0","id":6,"result":"0x6"}`, txt6)
			}()

			time.Sleep(10 * time.Millisecond)
		}
		wg.Wait()
	})

	t.Run("SingleObjectResponseForBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upCfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
				BatchMaxSize:  5,
				BatchMaxWait:  common.Duration(50 * time.Millisecond),
			},
		}
		network := setupTestNetworkSimple(t, ctx, upCfg, nil)
		err := network.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		// Upstream misbehaves and returns a single JSON object for a batched request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_blockNumber")
			}).
			Persist().
			Reply(429).
			BodyString(`{"code":-32007,"message":"300/second request limit reached - reduce calls per second or upgrade your account at quicknode.com"}`)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := network.Forward(ctx, req)
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded))
			}(i + 1)
		}
		wg.Wait()
	})
}

func TestNetwork_SingleRequestErrors(t *testing.T) {
	t.Run("SingleRequestUnauthorized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkSimple(t, ctx, nil, nil)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_blockNumber") }).
			Reply(401).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Unauthorized"}}`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err := network.Forward(ctx, req)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized))
		assert.Contains(t, err.Error(), "Unauthorized")
	})

	t.Run("SingleRequestUnsupported", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkSimple(t, ctx, nil, nil)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_unsupportedMethod") }).
			Reply(415).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_unsupportedMethod","params":[]}`))
		_, err := network.Forward(ctx, req)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported))
		assert.Contains(t, err.Error(), "Method not found")
	})

	t.Run("SingleRequestCapacityExceeded", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		upCfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				SupportsBatch: &common.TRUE,
				BatchMaxSize:  3,
				BatchMaxWait:  common.Duration(50 * time.Millisecond),
			},
		}
		network := setupTestNetworkSimple(t, ctx, upCfg, nil)
		err := network.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
		require.NoError(t, err)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_blockNumber") }).
			Reply(429).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Exceeded the quota"}}`)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := network.Forward(ctx, req)
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded))
			}(i + 1)
		}
		wg.Wait()
	})
}
