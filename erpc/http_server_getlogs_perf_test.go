package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func TestGetLogs_ChunkingAvoidsClientTTFBTimeout(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ConfigureTestLogger()

	// Upstream: latency proportional to block range to model "big getLogs too slow".
	perBlockDelay := 2 * time.Millisecond

	var getLogsCalls atomic.Int64
	var maxGetLogsRange atomic.Int64

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      json.RawMessage   `json:"id"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  result,
			})
		}

		switch req.Method {
		case "eth_chainId":
			reply("0x2105") // 8453
			return
		case "eth_syncing":
			reply(false)
			return
		case "eth_blockNumber":
			reply("0x100000")
			return
		case "eth_getBlockByNumber":
			reply(map[string]any{
				"number":    "0x100000",
				"timestamp": "0x1",
			})
			return
		case "eth_getLogs":
			getLogsCalls.Add(1)

			if len(req.Params) < 1 {
				reply([]any{})
				return
			}
			var filter map[string]any
			_ = json.Unmarshal(req.Params[0], &filter)

			fb, _ := filter["fromBlock"].(string)
			tb, _ := filter["toBlock"].(string)
			from, _ := common.HexToInt64(fb)
			to, _ := common.HexToInt64(tb)
			if from >= 0 && to >= from {
				rng := to - from + 1
				for {
					cur := maxGetLogsRange.Load()
					if rng <= cur || maxGetLogsRange.CompareAndSwap(cur, rng) {
						break
					}
				}
				delay := time.Duration(rng) * perBlockDelay
				timer := time.NewTimer(delay)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}

			reply([]any{
				map[string]any{"blockNumber": fb, "data": "0x"},
			})
			return
		default:
			// Keep poller/other calls fast and boring.
			reply(nil)
			return
		}
	}))
	defer up.Close()

	// Free local port, then start eRPC.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	host := "127.0.0.1"
	chunkSize := int64(1000)
	chunkConc := 10

	cfg := &common.Config{
		LogLevel: "warn",
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{
					{
						Id:     "mem",
						Driver: common.DriverMemory,
						Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "256MB"},
					},
				},
				Policies: []*common.CachePolicyConfig{
					{
						Connector: "mem",
						Network:   "*",
						Method:    "*",
						Finality:  common.DataFinalityStateFinalized,
						Empty:     common.CacheEmptyBehaviorAllow,
						AppliesTo: common.CachePolicyAppliesToBoth,
						TTL:       common.Duration(0),
					},
				},
			},
		},
		Server: &common.ServerConfig{
			HttpHostV4: util.StringPtr(host),
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: util.IntPtr(port),
			MaxTimeout: common.Duration(60 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "cache",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId:                      8453,
							GetLogsCacheChunkSize:        &chunkSize,
							GetLogsCacheChunkConcurrency: chunkConc,
							GetLogsSplitOnError:          util.BoolPtr(true),
							GetLogsSplitConcurrency:      3,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "up",
						Endpoint: up.URL,
						Type:     "evm",
						Evm:      &common.EvmUpstreamConfig{ChainId: 8453},
					},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Init(ctx, cfg, log.Logger)
	time.Sleep(250 * time.Millisecond)

	// Client timeout smaller than full-range upstream latency, larger than chunked latency.
	client := &http.Client{Timeout: 5 * time.Second}

	// range=8000 blocks. Without chunking => ~16s. With chunking to 8x1000 => ~2s wall.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3e8","toBlock":"0x2327","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}`)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/cache/evm/8453", host, port), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Less(t, time.Since(start), 5*time.Second)

	// Confirm upstream saw only chunk-sized sub-requests.
	require.GreaterOrEqual(t, getLogsCalls.Load(), int64(8))
	require.LessOrEqual(t, maxGetLogsRange.Load(), int64(1000))
}

func TestGetLogs_NoChunkingHitsClientTimeout(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ConfigureTestLogger()

	perBlockDelay := 2 * time.Millisecond

	var getLogsCalls atomic.Int64

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      json.RawMessage   `json:"id"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  result,
			})
		}

		switch req.Method {
		case "eth_chainId":
			reply("0x2105")
			return
		case "eth_syncing":
			reply(false)
			return
		case "eth_blockNumber":
			reply("0x100000")
			return
		case "eth_getBlockByNumber":
			reply(map[string]any{"number": "0x100000", "timestamp": "0x1"})
			return
		case "eth_getLogs":
			getLogsCalls.Add(1)

			if len(req.Params) < 1 {
				reply([]any{})
				return
			}
			var filter map[string]any
			_ = json.Unmarshal(req.Params[0], &filter)
			fb, _ := filter["fromBlock"].(string)
			tb, _ := filter["toBlock"].(string)
			from, _ := common.HexToInt64(fb)
			to, _ := common.HexToInt64(tb)
			if from >= 0 && to >= from {
				rng := to - from + 1
				delay := time.Duration(rng) * perBlockDelay
				timer := time.NewTimer(delay)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			reply([]any{map[string]any{"blockNumber": fb, "data": "0x"}})
			return
		default:
			reply(nil)
			return
		}
	}))
	defer up.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	host := "127.0.0.1"
	disabledChunk := int64(0)

	cfg := &common.Config{
		LogLevel: "warn",
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{
					{
						Id:     "mem",
						Driver: common.DriverMemory,
						Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "256MB"},
					},
				},
				Policies: []*common.CachePolicyConfig{
					{
						Connector: "mem",
						Network:   "*",
						Method:    "*",
						Finality:  common.DataFinalityStateFinalized,
						Empty:     common.CacheEmptyBehaviorAllow,
						AppliesTo: common.CachePolicyAppliesToBoth,
						TTL:       common.Duration(0),
					},
				},
			},
		},
		Server: &common.ServerConfig{
			HttpHostV4: util.StringPtr(host),
			ListenV4:   util.BoolPtr(true),
			HttpPortV4: util.IntPtr(port),
			MaxTimeout: common.Duration(60 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "cache",
				Networks: []*common.NetworkConfig{
					{
						Architecture: "evm",
						Evm: &common.EvmNetworkConfig{
							ChainId:               8453,
							GetLogsCacheChunkSize: &disabledChunk,
							GetLogsSplitOnError:   util.BoolPtr(true),
							GetLogsSplitConcurrency: 3,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "up",
						Endpoint: up.URL,
						Type:     "evm",
						Evm:      &common.EvmUpstreamConfig{ChainId: 8453},
					},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Init(ctx, cfg, log.Logger)
	time.Sleep(250 * time.Millisecond)

	client := &http.Client{Timeout: 5 * time.Second}

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3e8","toBlock":"0x2327","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}`)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/cache/evm/8453", host, port), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")

	_, err = client.Do(req)
	require.Error(t, err)
	// Some local environments return slightly different error text; just require timeout-ish.
	msg := err.Error()
	require.True(t, bytes.Contains([]byte(msg), []byte("timeout")) || bytes.Contains([]byte(msg), []byte("deadline")), msg)
	require.Greater(t, getLogsCalls.Load(), int64(0))
}
