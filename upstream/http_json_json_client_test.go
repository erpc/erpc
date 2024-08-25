package upstream

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

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
		assert.Contains(t, err.Error(), "invalid chars")
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
				assert.GreaterOrEqual(t, dur, 750*time.Millisecond)
				assert.Less(t, dur, 755*time.Millisecond)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "context deadline exceeded")
			}(i + 1)
		}
		wg.Wait()
	})
}
