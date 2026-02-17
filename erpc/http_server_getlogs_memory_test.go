package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Goal: prove no "unbounded" memory growth / OOM behavior for huge eth_getLogs upstream payloads.
// Mechanism under test:
// - upstream eth_getLogs decompressed size cap (GetLogsMaxResponseBytes)
// - split-on-too-large + recursive splitting until sub-requests become small enough
func TestGetLogs_UpstreamTooLarge_ResponseIsCappedAndSplit(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ConfigureTestLogger()

	// Config: small cap to force split; small-enough ranges succeed.
	const maxRespBytes = 1 << 20 // 1MiB
	const okRange = int64(25)

	var maxRangeSeen atomic.Int64
	var minRangeSeen atomic.Int64
	var getLogsCalls atomic.Int64
	minRangeSeen.Store(1 << 62)

	// Prebuild the large "data" chunk so the *server* doesn't dominate allocations for this test.
	dataChunk := bytes.Repeat([]byte("a"), 512)
	itemPrefix := []byte(`{"blockNumber":"0x1","data":"0x`)
	itemSuffix := []byte(`"}`)
	// Ensure we exceed maxRespBytes by a comfortable margin without writing 100s of MiB.
	oversizeItems := int(maxRespBytes/700) + 2000

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
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
			reply(map[string]any{"number": "0x100000", "timestamp": "0x1"})
			return
		case "eth_getLogs":
			getLogsCalls.Add(1)

			var filter map[string]any
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params[0], &filter)
			}
			fb, _ := filter["fromBlock"].(string)
			tb, _ := filter["toBlock"].(string)
			from, _ := common.HexToInt64(fb)
			to, _ := common.HexToInt64(tb)
			if from >= 0 && to >= from {
				rng := to - from + 1
				for {
					cur := maxRangeSeen.Load()
					if rng <= cur || maxRangeSeen.CompareAndSwap(cur, rng) {
						break
					}
				}
				for {
					cur := minRangeSeen.Load()
					if rng >= cur || minRangeSeen.CompareAndSwap(cur, rng) {
						break
					}
				}

				if rng > okRange {
					// Stream a huge valid JSON-RPC response body without buffering all of it in the handler.
					// Must exceed maxRespBytes so client hits ReadAllMax limit and returns TooLarge.
					w.Header().Set("content-type", "application/json")
					_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":[`, string(req.ID))
					// Write many entries; keep it valid JSON but oversized.
					for i := 0; i < oversizeItems; i++ {
						if i > 0 {
							_, _ = w.Write([]byte{','})
						}
						_, _ = w.Write(itemPrefix)
						_, _ = w.Write(dataChunk)
						_, _ = w.Write(itemSuffix)
					}
					_, _ = w.Write([]byte(`]}`))
					return
				}
			}

			// Small-range success payload
			reply([]any{map[string]any{"blockNumber": fb, "data": "0x"}})
			return
		default:
			reply(nil)
			return
		}
	}))
	defer up.CloseClientConnections()
	defer up.Close()

	// Free local port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	host := "127.0.0.1"
	chunkSize := int64(1000)
	chunkConc := 4

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
							GetLogsMaxResponseBytes:      maxRespBytes,
							GetLogsCacheChunkSize:        &chunkSize,
							GetLogsCacheChunkConcurrency: chunkConc,
							GetLogsSplitOnError:          util.BoolPtr(true),
							GetLogsSplitConcurrency:      2,
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

	// Force a GC so we can reason about post-run memory.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	client := &http.Client{Timeout: 30 * time.Second}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x3e8","toBlock":"0x2327","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}]}`)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/cache/evm/8453", host, port), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Assert: we hit an oversize response (needed splitting), and splitting reached <= okRange.
	require.Greater(t, getLogsCalls.Load(), int64(1))
	require.Greater(t, maxRangeSeen.Load(), okRange)
	require.LessOrEqual(t, minRangeSeen.Load(), okRange)

	// Assert: no runaway heap after GC (very loose bound; should be tiny in practice).
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	require.Less(t, heapDelta, int64(200*1024*1024), "heap delta too large; indicates retained buffers")
}

// Goal: prove single-block "too large" eth_getLogs does not hard-fail when it cannot be split further.
// Mechanism under test:
// - getLogs split-on-too-large
// - fallback to eth_getBlockReceipts + local filtering for a single block
func TestGetLogs_SingleBlockTooLarge_FallsBackToBlockReceipts(t *testing.T) {
	mainMutex.Lock()
	defer mainMutex.Unlock()

	util.ConfigureTestLogger()

	const maxRespBytes = 1 << 20 // 1MiB

	var getLogsCalls atomic.Int64
	var getBlockReceiptsCalls atomic.Int64

	dataChunk := bytes.Repeat([]byte("a"), 512)
	itemPrefix := []byte(`{"blockNumber":"0x1","data":"0x`)
	itemSuffix := []byte(`"}`)
	oversizeItems := int(maxRespBytes/700) + 2000

	transfer := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
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
			reply(map[string]any{"number": "0x1", "timestamp": "0x1"})
			return
		case "eth_getLogs":
			getLogsCalls.Add(1)
			// Always respond with an oversized valid JSON-RPC response body for the single block.
			w.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":[`, string(req.ID))
			for i := 0; i < oversizeItems; i++ {
				if i > 0 {
					_, _ = w.Write([]byte{','})
				}
				_, _ = w.Write(itemPrefix)
				_, _ = w.Write(dataChunk)
				_, _ = w.Write(itemSuffix)
			}
			_, _ = w.Write([]byte(`]}`))
			return
		case "eth_getBlockReceipts":
			getBlockReceiptsCalls.Add(1)
			// Return receipts with logs; include both matching and non-matching topics.
			reply([]any{
				map[string]any{
					"transactionIndex": "0x0",
					"logs": []any{
						map[string]any{
							"address":          "0x0000000000000000000000000000000000000001",
							"topics":           []any{transfer},
							"data":             "0x",
							"blockNumber":      "0x1",
							"transactionIndex": "0x0",
							"logIndex":         "0x0",
						},
						map[string]any{
							"address":          "0x0000000000000000000000000000000000000002",
							"topics":           []any{"0xdeadbeef"},
							"data":             "0x",
							"blockNumber":      "0x1",
							"transactionIndex": "0x0",
							"logIndex":         "0x1",
						},
					},
				},
				map[string]any{
					"transactionIndex": "0x1",
					"logs": []any{
						map[string]any{
							"address":          "0x0000000000000000000000000000000000000003",
							"topics":           []any{transfer},
							"data":             "0x",
							"blockNumber":      "0x1",
							"transactionIndex": "0x1",
							"logIndex":         "0x2",
						},
					},
				},
			})
			return
		default:
			reply(nil)
			return
		}
	}))
	defer up.CloseClientConnections()
	defer up.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	host := "127.0.0.1"
	chunkSize := int64(1000)

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
							GetLogsMaxResponseBytes:      maxRespBytes,
							GetLogsCacheChunkSize:        &chunkSize,
							GetLogsCacheChunkConcurrency: 4,
							GetLogsSplitOnError:          util.BoolPtr(true),
							GetLogsSplitConcurrency:      2,
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

	client := &http.Client{Timeout: 30 * time.Second}
	body := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x1","topics":["%s"]}]}`, transfer))
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/cache/evm/8453", host, port), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Result []any `json:"result"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Result, 2)

	require.Equal(t, int64(1), getLogsCalls.Load(), "expected single too-large eth_getLogs call")
	require.Equal(t, int64(1), getBlockReceiptsCalls.Load(), "expected fallback eth_getBlockReceipts call")
}
