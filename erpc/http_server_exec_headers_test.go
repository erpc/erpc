package erpc

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecutionHeaders_All_FullTrace verifies that the default mode
// emits the unified X-ERPC-Upstreams participation log alongside the
// per-scope counter headers.
func TestExecutionHeaders_All_FullTrace(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Times(1).
		Reply(500).
		BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`)
	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x42"}`)

	cfg := minimalTwoUpstreamCfg(t, common.ExecutionHeadersAll)
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, headers, body := sendRequest(
		`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`,
		nil, nil,
	)
	require.Equal(t, 200, statusCode)
	assert.Contains(t, body, `"result":"0x42"`)

	// Build-identity headers present when not 'off'.
	assert.NotEmpty(t, headers["X-Erpc-Version"], "version header should be present when not 'off'")
	assert.NotEmpty(t, headers["X-Erpc-Commit"], "commit header should be present when not 'off'")
	// Summary counters always present.
	assert.Equal(t, "MISS", headers["X-Erpc-Cache"])
	assert.NotEmpty(t, headers["X-Erpc-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Duration"])
	// Per-scope counters.
	assert.NotEmpty(t, headers["X-Erpc-Upstream-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Network-Attempts"])

	// Unified participation log. Each segment is
	// `<id>=<reason>:<outcome>:<duration>ms[:won]`.
	ups := headers["X-Erpc-Upstreams"]
	require.NotEmpty(t, ups, "expected X-ERPC-Upstreams in 'all' mode")
	segments := strings.Split(ups, ";")
	assert.GreaterOrEqual(t, len(segments), 1)
	// At least one segment carries :won (the rpc2 success).
	wonCount := 0
	for _, s := range segments {
		if strings.HasSuffix(s, ":won") {
			wonCount++
		}
		// Format invariant: id=reason:outcome:durationms[:won]
		parts := strings.SplitN(s, "=", 2)
		require.Lenf(t, parts, 2, "segment %q missing '='", s)
		fields := strings.Split(parts[1], ":")
		// reason : outcome : duration[ms] : (optional won)
		require.GreaterOrEqualf(t, len(fields), 3, "segment %q has fewer than 3 fields", s)
		assert.Truef(t, strings.HasSuffix(fields[2], "ms"), "duration field %q must end with ms", fields[2])
	}
	assert.GreaterOrEqual(t, wonCount, 1, "expected at least one :won segment in %q", ups)
}

// TestExecutionHeaders_Summary_OmitsSliceHeaders verifies "summary"
// mode keeps the counter triplet but drops the participation log.
func TestExecutionHeaders_Summary_OmitsSliceHeaders(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	cfg := minimalTwoUpstreamCfg(t, common.ExecutionHeadersSummary)
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, headers, _ := sendRequest(
		`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`,
		nil, nil,
	)
	require.Equal(t, 200, statusCode)

	// Counter headers + per-scope counters stay.
	assert.NotEmpty(t, headers["X-Erpc-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Cache"])
	assert.NotEmpty(t, headers["X-Erpc-Upstream-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Network-Attempts"])
	// The participation log is dropped in summary mode.
	assert.Empty(t, headers["X-Erpc-Upstreams"])
}

// TestExecutionHeaders_Off_NoDiagnosticHeaders verifies "off" mode
// suppresses every X-ERPC-* diagnostic header.
func TestExecutionHeaders_Off_NoDiagnosticHeaders(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	cfg := minimalTwoUpstreamCfg(t, common.ExecutionHeadersOff)
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, headers, _ := sendRequest(
		`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`,
		nil, nil,
	)
	require.Equal(t, 200, statusCode)

	for k := range headers {
		assert.Falsef(t, strings.HasPrefix(strings.ToLower(k), "x-erpc-"),
			"no X-ERPC-* header may be emitted in 'off' mode, found %q", k)
	}
}

func minimalTwoUpstreamCfg(t *testing.T, mode common.ExecutionHeadersMode) *common.Config {
	_ = log.Logger
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout:       common.Duration(5 * time.Second).Ptr(),
			ExecutionHeaders: &mode,
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
						Failsafe: []*common.FailsafeConfig{
							{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id: "rpc1", Type: common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
					{
						Id: "rpc2", Type: common.UpstreamTypeEvm,
						Endpoint: "http://rpc2.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}
