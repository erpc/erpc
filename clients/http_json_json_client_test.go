package clients

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func TestHttpJsonRpcClient_SingleRequests(t *testing.T) {
	logger := log.Logger

	t.Run("TimeoutError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		dur := 1 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), dur)
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"result": "0x1"})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(ctx, req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "remote endpoint request timeout")
	})

	t.Run("ServerNotRespondingEmptyBody", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(504).
			BodyString("")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(ctx, req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "504")
	})

	t.Run("IncompleteResponse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)

		assert.Error(t, err)
	})

	t.Run("TraceExecutionTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":[{"error":"execution timeout"}]}`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"debug_traceBlockByNumber","params":["0x226AECC",{"tracer":"callTracer","timeout":"1ms"}],"id":1}`))
		_, err = client.SendRequest(context.Background(), req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "-32015")
		assert.Contains(t, err.Error(), "execution timeout")
	})
}

func TestHttpJsonRpcClient_BatchRequests(t *testing.T) {
	logger := log.Logger

	t.Run("SimpleBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", nil, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, &common.JsonRpcUpstreamConfig{
			SupportsBatch: &[]bool{true}[0],
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				if err != nil {
					assert.NotContains(t, err.Error(), "no response received for request")
				}
				assert.NoError(t, err)
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("ConcurrentRequestsRaceCondition", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  2,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Times(3).
			Reply(200).
			Delay(50 * time.Millisecond).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"},{"jsonrpc":"2.0","id":4,"result":"0x4"},{"jsonrpc":"2.0","id":5,"result":"0x5"},{"jsonrpc":"2.0","id":6,"result":"0x6"}]`)

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, i+1)))
				_, err := client.SendRequest(context.Background(), req)
				if err != nil {
					assert.NotContains(t, err.Error(), "no response received for request")
				}
				assert.NoError(t, err)
			}()
		}
		wg.Wait()
	})

	t.Run("SeparateBatchRequestsWithSameIDs", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", nil, &url.URL{Scheme: "http", Host: "rpc1.localhost"}, &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(500 * time.Millisecond),
		}, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(5).
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":6,"result":"0x6"}]`)

		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req1 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
				resp1, err1 := client.SendRequest(context.Background(), req1)
				assert.NoError(t, err1)
				if resp1 == nil {
					panic(fmt.Sprintf("SeparateBatchRequestsWithSameIDs: resp1 is nil err1: %v", err1))
				}
				wr := bytes.NewBuffer([]byte{})
				_, werr := resp1.WriteTo(wr)
				assert.NoError(t, werr)
				txt := wr.String()
				assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, txt)
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				req6 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":6,"method":"eth_blockNumber","params":[]}`))
				resp6, err6 := client.SendRequest(context.Background(), req6)
				assert.NoError(t, err6)
				wr := bytes.NewBuffer([]byte{})
				_, werr := resp6.WriteTo(wr)
				assert.NoError(t, werr)
				txt := wr.String()
				assert.Equal(t, `{"jsonrpc":"2.0","id":6,"result":"0x6"}`, txt)
			}()
			time.Sleep(10 * time.Millisecond)
		}
		wg.Wait()

		assert.True(t, gock.IsDone())
	})

	t.Run("RequestEndpointTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))

		_, err = client.SendRequest(ctx, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "remote endpoint request timeout")
	})

	t.Run("RequestContextTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON([]map[string]interface{}{
				{"jsonrpc": "2.0", "id": 1, "result": "0x1"},
				{"jsonrpc": "2.0", "id": 2, "result": "0x2"},
			})

		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
				defer cancel()
				_, err := client.SendRequest(ctx, req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "remote endpoint request timeout")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SingleErrorForBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      nil,
				"error":   map[string]interface{}{"code": -32000, "message": "Server error"},
			})

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "Server error")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("HTMLResponseForBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(503).
			BodyString("<html><body><h1>503 Service Unavailable</h1></body></html>")

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "503 Service Unavailable")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SingleStringResponseForBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString("my random something error")

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "my random something error")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SingleObjectResponseForBatchRequest", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  5,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(429).
			BodyString(`{"code":-32007,"message":"300/second request limit reached - reduce calls per second or upgrade your account at quicknode.com"}`)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				assert.Error(t, err)
				txt, _ := sonic.Marshal(err)
				assert.Contains(t, string(txt), "ErrEndpointCapacityExceeded")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SingleRequestUnauthorized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(401).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "Unauthorized"},
			})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Unauthorized")
		assert.IsType(t, &common.ErrEndpointUnauthorized{}, err)
	})

	t.Run("SingleRequestUnsupported", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(415).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32601, "message": "Method not found"},
			})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_unsupportedMethod","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "Method not found")
		assert.IsType(t, &common.ErrEndpointUnsupported{}, err)
	})

	t.Run("SingleRequestCapacityExceeded", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  3,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(429).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32005, "message": "Exceeded the quota"},
			})

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(context.Background(), req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "ErrEndpointCapacityExceeded")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("PartialBatchResponse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  3,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"}]`)

		var wg sync.WaitGroup
		results := make([]interface{}, 3)
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				resp, err := client.SendRequest(context.Background(), req)
				if err != nil {
					results[id-1] = fmt.Sprintf("Error: %v", err)
				} else {
					results[id-1] = resp
				}
			}(i + 1)
		}
		wg.Wait()

		assert.Len(t, results, 3)
		assert.NotNil(t, results[0])
		assert.NotNil(t, results[1])
		assert.Contains(t, results[2].(string), "no response received for request")
	})

	t.Run("BatchRequestTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  3,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
		}
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`)

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
				defer cancel()
				_, err := client.SendRequest(ctx, req)
				dur := time.Since(start)
				assert.Greater(t, dur, 740*time.Millisecond)
				assert.Less(t, dur, 780*time.Millisecond)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "remote endpoint request timeout")
			}(i + 1)
		}
		wg.Wait()
	})
	t.Run("CustomHeaders", func(t *testing.T) {
		defer gock.Off()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := zerolog.Nop()

		// Define custom headers
		customHeaders := map[string]string{
			"Authorization":    "Bearer TEST_TOKEN_123",
			"X-Custom-Header":  "CustomValue",
			"X-Another-Header": "AnotherValue",
		}

		// Create a fake upstream config with the headers
		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.TRUE,
			BatchMaxSize:  3,
			BatchMaxWait:  common.Duration(50 * time.Millisecond),
			Headers:       customHeaders,
		}

		// Initialize the JSON-RPC client
		client, err := NewGenericHttpJsonRpcClient(
			ctx,
			&logger,
			"prj1",
			ups,
			&url.URL{Scheme: "http", Host: "rpc1.localhost:8545"},
			ups.Config().JsonRpc,
			nil,
		)
		assert.NoError(t, err)

		// Use Gock to intercept the outbound HTTP request, checking headers
		gock.New("http://rpc1.localhost:8545").
			Post("/").
			MatchHeader("Authorization", "Bearer TEST_TOKEN_123").
			MatchHeader("X-Custom-Header", "CustomValue").
			MatchHeader("X-Another-Header", "AnotherValue").
			Reply(200).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`)

		// Create a NormalizedRequest for JSON-RPC
		req := common.NewNormalizedRequest([]byte(`{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "eth_blockNumber",
            "params": []
        }`))

		// Send the request
		resp, err := client.SendRequest(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		// Extract the JsonRpcResponse fields
		jr, err := resp.JsonRpcResponse()
		assert.NoError(t, err)
		assert.NotNil(t, jr, "Expected a valid JSON-RPC response object")

		// Confirm all Gock interceptors were called and matched
		assert.True(t, gock.IsDone(), "Expected Gock to intercept and match the outbound request")
	})

	t.Run("ProxyPool", func(t *testing.T) {
		defer gock.Off()
		logger := zerolog.Nop()

		// build the proxy pool registry
		proxyCfg := []*common.ProxyPoolConfig{
			{
				ID:   "pool1",
				Urls: []string{"http://myproxy1:8080"},
			},
		}
		proxyRegistry, err := NewProxyPoolRegistry(proxyCfg, &logger)
		assert.NoError(t, err, "expected no error creating ProxyPoolRegistry")

		// create a fake upstream and reference the proxy pool
		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{
			ProxyPool: "pool1",
		}

		// lookup the pool directly
		proxyPool, err := proxyRegistry.GetPool("pool1")
		assert.NoError(t, err, "expected no error getting pool")

		// create the json-rpc client
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client, err := NewGenericHttpJsonRpcClient(
			ctx,
			&logger,
			"prj1",
			ups,
			&url.URL{Scheme: "http", Host: "rpc1.localhost:8545"},
			ups.Config().JsonRpc,
			proxyPool,
		)
		assert.NoError(t, err, "expected no error creating JsonRpcClient")

		// get the underlying http client
		actual, ok := client.(*GenericHttpJsonRpcClient)
		assert.True(t, ok, "expected client to be *GenericHttpJsonRpcClient")

		// inspect the transport's proxy
		httpClient := actual.getHttpClient()
		assert.NotNil(t, httpClient, "expected a valid *http.Client from UnderlyingHttpClient")

		transport, ok := httpClient.Transport.(*http.Transport)
		assert.True(t, ok, "expected transport to be an *http.Transport")

		proxyFunc := transport.Proxy
		assert.NotNil(t, proxyFunc, "expected a Proxy func in transport")

		// verify the proxy is indeed set
		req, _ := http.NewRequest("GET", "http://rpc1.localhost:8545", nil)
		proxiedUrl, err := proxyFunc(req)
		assert.NoError(t, err, "expected no error from proxy func")
		assert.NotNil(t, proxiedUrl, "expected a non-nil proxy URL")
		assert.Equal(t, "http", proxiedUrl.Scheme)
		assert.Equal(t, "myproxy1:8080", proxiedUrl.Host)
	})

}
func TestHttpJsonRpcClient_CapacityExceededErrors(t *testing.T) {
	logger := log.Logger

	t.Run("Direct429Response", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		// Set up a 429 response with an empty body
		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(429).
			BodyString("")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)

		// Verify the error
		assert.Error(t, err)

		// Check that we got a capacity exceeded error
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "Expected ErrEndpointCapacityExceeded, got: %T: %v", err, err)

		// Verify the underlying JSON-RPC error code
		capacityErr, ok := err.(*common.ErrEndpointCapacityExceeded)
		assert.True(t, ok, "Expected ErrEndpointCapacityExceeded, got: %T", err)

		// Verify the status code was preserved in the details
		cause, ok := capacityErr.Cause.(*common.ErrJsonRpcExceptionInternal)
		assert.True(t, ok, "Expected ErrJsonRpcExceptionInternal, got: %T", capacityErr.Cause)
		details := cause.Details
		assert.Equal(t, 429, details["statusCode"])
	})

	t.Run("429WithErrorMessage", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ups := common.NewFakeUpstream("rpc1")
		ups.Config().Type = common.UpstreamTypeEvm
		ups.Config().Endpoint = "http://rpc1.localhost:8545"
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "prj1", ups, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"}, ups.Config().JsonRpc, nil)
		assert.NoError(t, err)

		// Set up a 429 response with a rate limit error message
		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(429).
			BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Rate limit exceeded"}}`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)

		// Verify the error
		assert.Error(t, err)

		// Check that we got a capacity exceeded error
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "Expected ErrEndpointCapacityExceeded, got: %T: %v", err, err)

		// Verify the error message was preserved
		assert.Contains(t, err.Error(), "Rate limit exceeded")
	})
}
