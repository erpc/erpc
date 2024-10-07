package upstream

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestHttpJsonRpcClient_NoResponseErrors(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())

	t.Run("TimeoutError", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"result": "0x1"})

		dur := 1 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), dur)
		defer cancel()

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(ctx, req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "remote endpoint request timeout")
	})

	t.Run("ServerNotRespondingEmptyBody", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(504).
			BodyString("")

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "504")
		assert.Contains(t, err.Error(), "504")
	})

	t.Run("IncompleteResponse", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			BodyString(`{"jsonrpc":"2.0","id":1,"result":`)

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		_, err = client.SendRequest(context.Background(), req)

		assert.Error(t, err)
	})

	t.Run("ConcurrentRequestsRaceCondition", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  2,
					BatchMaxWait:  "10ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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

	t.Run("RequestMultiplexingIssue", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &[]bool{true}[0],
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
}

func TestHttpJsonRpcClient_BatchRequests(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())

	t.Run("SeparateBatchRequestsWithSameIDs", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "500ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost"})
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
				rdr, werr := resp1.GetReader()
				assert.NoError(t, werr)
				wr.ReadFrom(rdr)
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
				rdr, werr := resp6.GetReader()
				wr.ReadFrom(rdr)
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		_, err = client.SendRequest(ctx, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "remote endpoint request timeout")
	})

	t.Run("RequestContextTimeout", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(2 * time.Second).
			JSON([]map[string]interface{}{
				{"jsonrpc": "2.0", "id": 1, "result": "0x1"},
				{"jsonrpc": "2.0", "id": 2, "result": "0x2"},
			})

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				_, err := client.SendRequest(ctx, req)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "remote endpoint request timeout")
			}(i + 1)
		}
		wg.Wait()
	})

	t.Run("SingleErrorForBatchRequest", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  5,
					BatchMaxWait:  "50ms",
				},
				VendorName: "quicknode",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &common.TRUE,
					BatchMaxSize:  3,
					BatchMaxWait:  "50ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
}

func TestHttpJsonRpcClient_BatchRequestErrors(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())

	t.Run("PartialBatchResponse", func(t *testing.T) {
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &[]bool{true}[0],
					BatchMaxSize:  3,
					BatchMaxWait:  "10ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
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
		defer gock.Off()

		client, err := NewGenericHttpJsonRpcClient(&logger, &Upstream{
			config: &common.UpstreamConfig{
				Endpoint: "http://rpc1.localhost:8545",
				JsonRpc: &common.JsonRpcUpstreamConfig{
					SupportsBatch: &[]bool{true}[0],
					BatchMaxSize:  3,
					BatchMaxWait:  "500ms",
				},
			},
		}, &url.URL{Scheme: "http", Host: "rpc1.localhost:8545"})
		assert.NoError(t, err)

		gock.New("http://rpc1.localhost:8545").
			Post("/").
			Reply(200).
			Delay(1 * time.Second).
			BodyString(`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`)

		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()

		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_blockNumber","params":[]}`, id)))
				start := time.Now()
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
}
