package erpc

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipE2ECfg builds a full-stack config: real HTTP server, one EVM upstream
// (rpc1.localhost, mocked by gock), enableGzip on, and a finalized-block
// memory cache so the #990 "cache HIT served uncompressed" symptom is
// reproducible end to end.
func gzipE2ECfg() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			EnableGzip: util.BoolPtr(true),
			ListenV4:   util.BoolPtr(true),
		},
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{
					{
						Id:     "mem",
						Driver: common.DriverMemory,
						Memory: &common.MemoryConnectorConfig{
							MaxItems: 100_000, MaxTotalSize: "1GB",
						},
					},
				},
				Policies: []*common.CachePolicyConfig{
					{
						Network:   "*",
						Method:    "*",
						Finality:  common.DataFinalityStateFinalized,
						Connector: "mem",
						TTL:       common.FixedDuration(5 * time.Minute),
					},
				},
			},
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
						Failsafe: []*common.FailsafeConfig{
							{Retry: &common.RetryPolicyConfig{MaxAttempts: 2}},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id: "rpc1", Type: common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// TestHttpServer_Gzip_E2E_StreamedJsonRpc is the end-to-end regression for
// https://github.com/erpc/erpc/issues/990. It boots the real HTTP server over
// a TCP socket and drives a real HTTP client, so the response travels the full
// production path: routing -> upstream client -> NormalizedResponse.WriteTo ->
// JsonRpcResponse.WriteTo (envelope prefix write, then the large result write)
// -> gzipHandler/conditionalGzipWriter -> the wire.
//
// Setting Accept-Encoding: gzip explicitly disables Go's transparent
// decompression, so the test observes the actual Content-Encoding header and
// the raw compressed bytes on the wire.
func TestHttpServer_Gzip_E2E_StreamedJsonRpc(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		// Real call for the client<->server hop (localhost); intercept upstream.
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()

	// A large finalized block (0x386053 << finalized tip 0x11117777), shaped
	// like the multi-MB payloads #990 is about. ~130KB, far above the 1KB
	// compressionThreshold, streamed after the ~22-byte envelope prefix.
	const sentinel = "0xda7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7ada7a"
	txs := make([]string, 0, 2000)
	for i := range 2000 {
		txs = append(txs, fmt.Sprintf(`"%s%04x"`, sentinel[:60], i))
	}
	bigResult := fmt.Sprintf(
		`{"number":"0x386053","hash":"%s","transactions":[%s]}`,
		sentinel, strings.Join(txs, ","),
	)
	expectedBody := `{"jsonrpc":"2.0","id":1,"result":` + bigResult + `}`

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "0x386053")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + bigResult + `}`))

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(gzipE2ECfg(), t)
	defer shutdown()

	reqBody := `{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x386053",false],"id":1}`

	// First call: served from upstream (cache MISS).
	status, headers, raw := sendRequest(reqBody, map[string]string{"Accept-Encoding": "gzip"}, nil)
	require.Equal(t, http.StatusOK, status, "body: %.200s", raw)
	assert.Equal(t, "gzip", headers["Content-Encoding"], "large JSON-RPC response must be gzip-compressed (issue #990)")
	assert.Equal(t, "Accept-Encoding", headers["Vary"])

	decoded := gunzipE2E(t, raw)
	assert.Equal(t, expectedBody, decoded, "decompressed body must equal the full JSON-RPC response")
	assert.Less(t, len(raw), len(decoded), "wire bytes must be smaller than the uncompressed payload")
	t.Logf("MISS: wire=%d bytes, decompressed=%d bytes, ratio=%.3f, x-erpc-cache=%q",
		len(raw), len(decoded), float64(len(raw))/float64(len(decoded)), headers["X-Erpc-Cache"])

	// Second call: served from the finalized-block cache (HIT). #990 explicitly
	// reported cache hits going out uncompressed; assert the HIT path compresses
	// too. The HIT re-serializes through the same WriteTo -> gzip path.
	status2, headers2, raw2 := sendRequest(reqBody, map[string]string{"Accept-Encoding": "gzip"}, nil)
	require.Equal(t, http.StatusOK, status2)
	assert.Equal(t, "HIT", headers2["X-Erpc-Cache"], "second identical finalized request should be a cache hit")
	assert.Equal(t, "gzip", headers2["Content-Encoding"], "cache-HIT large response must also be gzip-compressed (issue #990)")
	assert.Equal(t, expectedBody, gunzipE2E(t, raw2))
}

// TestHttpServer_Gzip_E2E_SmallResponseUncompressed confirms the CPU-saving
// behavior is preserved end to end: sub-threshold responses stay plain.
func TestHttpServer_Gzip_E2E_SmallResponseUncompressed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(gzipE2ECfg(), t)
	defer shutdown()

	status, headers, body := sendRequest(
		`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`,
		map[string]string{"Accept-Encoding": "gzip"}, nil,
	)
	require.Equal(t, http.StatusOK, status, "body: %s", body)
	assert.Empty(t, headers["Content-Encoding"], "small responses must stay uncompressed")
	assert.Equal(t, "Accept-Encoding", headers["Vary"])
	assert.Contains(t, body, `"result":"0x1"`)
}

func gunzipE2E(t *testing.T, s string) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader([]byte(s)))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return string(out)
}
