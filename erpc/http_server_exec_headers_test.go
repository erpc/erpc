package erpc

import (
	"net/http"
	"strconv"
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
// emits the full X-ERPC-Upstreams-* slice headers describing every
// participant the executor touched.
func TestExecutionHeaders_All_FullTrace(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// We use Persist() above; no asserts on pending mocks.

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

	// Summary counters always present.
	assert.Equal(t, "MISS", headers["X-Erpc-Cache"])
	assert.NotEmpty(t, headers["X-Erpc-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Retries"])
	assert.NotEmpty(t, headers["X-Erpc-Hedges"])
	assert.NotEmpty(t, headers["X-Erpc-Duration"])
	// Network-scope counters.
	assert.NotEmpty(t, headers["X-Erpc-Network-Attempts"])

	// Per-attempt trace headers.
	tried := headers["X-Erpc-Upstreams-Tried"]
	outcomes := headers["X-Erpc-Upstreams-Outcomes"]
	reasons := headers["X-Erpc-Upstreams-Reasons"]
	durations := headers["X-Erpc-Upstreams-Durations-Ms"]
	flags := headers["X-Erpc-Upstreams-Flags"]
	require.NotEmpty(t, tried, "expected per-attempt trace headers in 'all' mode")
	require.NotEmpty(t, outcomes)
	require.NotEmpty(t, reasons)
	require.NotEmpty(t, durations)
	require.NotEmpty(t, flags)

	// Slice lengths must line up across all per-attempt headers.
	triedN := len(strings.Split(tried, ","))
	assert.Equal(t, triedN, len(strings.Split(outcomes, ",")))
	assert.Equal(t, triedN, len(strings.Split(reasons, ",")))
	assert.Equal(t, triedN, len(strings.Split(durations, ",")))
	assert.Equal(t, triedN, len(strings.Split(flags, ",")))

	// At least one attempt is in the trace, and at least one outcome
	// is "success" since the request returned 200.
	assert.GreaterOrEqual(t, triedN, 1, "expected at least one attempt in trace")
	assert.Contains(t, outcomes, "success")

	// Durations are integers (ms).
	for _, d := range strings.Split(durations, ",") {
		_, err := strconv.ParseInt(d, 10, 64)
		require.NoError(t, err, "duration %q must be integer ms", d)
	}
}

// TestExecutionHeaders_Summary_OmitsSliceHeaders verifies "summary"
// mode keeps the counter triplet but drops the per-attempt slices.
func TestExecutionHeaders_Summary_OmitsSliceHeaders(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// We use Persist() above; no asserts on pending mocks.

	// Persist matchers so whichever upstream the registry rotates to
	// receives a deterministic answer (we only care about headers here).
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

	assert.NotEmpty(t, headers["X-Erpc-Attempts"])
	assert.NotEmpty(t, headers["X-Erpc-Retries"])
	assert.NotEmpty(t, headers["X-Erpc-Hedges"])
	assert.NotEmpty(t, headers["X-Erpc-Cache"])
	// Slice headers must NOT be present.
	assert.Empty(t, headers["X-Erpc-Upstreams-Tried"])
	assert.Empty(t, headers["X-Erpc-Upstreams-Outcomes"])
	assert.Empty(t, headers["X-Erpc-Upstreams-Reasons"])
	assert.Empty(t, headers["X-Erpc-Upstreams-Durations-Ms"])
	assert.Empty(t, headers["X-Erpc-Network-Attempts"])
}

// TestExecutionHeaders_Off_NoDiagnosticHeaders verifies "off" mode
// suppresses every X-ERPC-* diagnostic header.
func TestExecutionHeaders_Off_NoDiagnosticHeaders(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// We use Persist() above; no asserts on pending mocks.

	// Persist matchers so whichever upstream the registry rotates to
	// receives a deterministic answer (we only care about headers here).
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
		assert.False(t, strings.HasPrefix(k, "X-Erpc-Cache") ||
			strings.HasPrefix(k, "X-Erpc-Upstream") ||
			strings.HasPrefix(k, "X-Erpc-Attempts") ||
			strings.HasPrefix(k, "X-Erpc-Retries") ||
			strings.HasPrefix(k, "X-Erpc-Hedges") ||
			strings.HasPrefix(k, "X-Erpc-Duration") ||
			strings.HasPrefix(k, "X-Erpc-Network-") ||
			strings.HasPrefix(k, "X-Erpc-Consensus-"),
			"diagnostic header %q must be suppressed in 'off' mode", k,
		)
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
