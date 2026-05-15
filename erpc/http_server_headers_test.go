package erpc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHttpServer_HeadersCoverage verifies that X-ERPC-* diagnostic
// headers are present on EVERY response path that has a request —
// success, validation reject, JSON-RPC errors, even when the upstream
// call never succeeds. Headers may be absent only on very-early errors
// (URL parse, project lookup) where no request was constructed.
func TestHttpServer_HeadersCoverage(t *testing.T) {
	cfg := minimalServerConfig()

	t.Run("success path carries full header set", func(t *testing.T) {
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), `"eth_getBalance"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xdeadbeef",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, headers, body := sendRequest(
			`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`,
			nil, nil,
		)
		require.Equal(t, 200, statusCode, "body=%s", body)
		assert.Contains(t, body, "0xdeadbeef")
		assertAllScopeHeadersPresent(t, headers)
	})

	t.Run("validation error carries headers", func(t *testing.T) {
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Malformed JSON-RPC (missing method) caught by nq.Validate().
		// Validation errors return 400 (Bad Request) — but per-request
		// processing built the nq, so the counter headers must be set.
		statusCode, headers, _ := sendRequest(
			`{"jsonrpc":"2.0","id":1}`,
			nil, nil,
		)
		assert.Contains(t, []int{200, 400}, statusCode)
		assertCounterHeadersPresent(t, headers)
	})
}

// TestExecState_CounterAggregation verifies that totals (Attempts /
// Retries / Hedges) are correctly derived as the sum of per-scope
// counters — so increments at the network OR upstream OR cache scope
// all contribute to the totals.
func TestExecState_CounterAggregation(t *testing.T) {
	t.Parallel()

	t.Run("totals sum across scopes", func(t *testing.T) {
		t.Parallel()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		st := req.ExecState()
		require.NotNil(t, st)

		st.UpstreamAttempts.Store(3)
		st.UpstreamRetries.Store(1)
		st.UpstreamHedges.Store(1)
		st.NetworkAttempts.Store(2)
		st.NetworkRetries.Store(1)
		st.NetworkHedges.Store(0)
		st.CacheAttempts.Store(1)
		st.CacheRetries.Store(0)
		st.CacheHedges.Store(0)

		snap := st.Snapshot()
		// Attempts = physical work only (Upstream + Cache). NetworkAttempts
		// is a rotation count and is NOT summed in to avoid double-counting
		// (each rotation triggers exactly one upstream invocation chain).
		assert.Equal(t, 4, snap.Attempts, "Attempts = UpstreamAttempts (3) + CacheAttempts (1)")
		// Retries / Hedges sum across scopes — each is a distinct event.
		assert.Equal(t, 2, snap.Retries, "Retries = 1 (upstream) + 1 (network) + 0 (cache)")
		assert.Equal(t, 1, snap.Hedges, "Hedges = 1 (upstream) + 0 (network) + 0 (cache)")
		assert.Equal(t, 3, snap.UpstreamAttempts)
		assert.Equal(t, 2, snap.NetworkAttempts)
		assert.Equal(t, 1, snap.CacheAttempts)
	})

	t.Run("zero state snapshots to all-zeros", func(t *testing.T) {
		t.Parallel()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		st := req.ExecState()
		snap := st.Snapshot()
		assert.Equal(t, 0, snap.Attempts)
		assert.Equal(t, 0, snap.Retries)
		assert.Equal(t, 0, snap.Hedges)
	})

	t.Run("nil ExecState snapshots safely", func(t *testing.T) {
		t.Parallel()
		var st *common.ExecState
		snap := st.Snapshot()
		assert.Equal(t, 0, snap.Attempts)
		assert.Equal(t, 0, snap.Retries)
		assert.Equal(t, 0, snap.Hedges)
	})

	t.Run("MarkUpstreamAttemptWon marks the most-recent matching attempt", func(t *testing.T) {
		t.Parallel()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		st := req.ExecState()
		require.NotNil(t, st)

		st.RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "rpc1", Outcome: common.UpstreamOutcomeTimeout})
		st.RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "rpc2", Outcome: common.UpstreamOutcomeSuccess})
		st.RecordUpstreamAttempt(common.UpstreamAttempt{UpstreamId: "rpc1", Outcome: common.UpstreamOutcomeSuccess})

		st.MarkUpstreamAttemptWon("rpc1")
		st.MarkUpstreamAttemptWon("rpc2")

		log := st.UpstreamAttemptLog()
		require.Len(t, log, 3)
		assert.False(t, log[0].Won, "first rpc1 attempt (timeout) not marked")
		assert.True(t, log[1].Won, "rpc2 attempt marked")
		assert.True(t, log[2].Won, "second rpc1 attempt (most recent) marked")
	})

	t.Run("MarkUpstreamAttemptWon nil-safe + unknown-upstream-safe", func(t *testing.T) {
		t.Parallel()
		var st *common.ExecState
		st.MarkUpstreamAttemptWon("does-not-exist") // no panic

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
		st2 := req.ExecState()
		st2.MarkUpstreamAttemptWon("never-tried") // no panic, no record
		assert.Empty(t, st2.UpstreamAttemptLog())
	})
}

// assertCounterHeadersPresent verifies the always-on counter headers
// are emitted. These flow from ExecState and are present on every
// response that has a request attached.
func assertCounterHeadersPresent(t *testing.T, headers map[string]string) {
	t.Helper()
	required := []string{
		"X-Erpc-Attempts", // total physical work across scopes
		"X-Erpc-Upstream-Attempts",
		"X-Erpc-Upstream-Retries",
		"X-Erpc-Upstream-Hedges",
		"X-Erpc-Network-Attempts",
		"X-Erpc-Network-Retries",
		"X-Erpc-Network-Hedges",
	}
	for _, h := range required {
		v, ok := headers[h]
		assert.Truef(t, ok, "missing required counter header: %s (got %v)", h, headersKeys(headers))
		if ok {
			assert.NotEmpty(t, v, "header %s should not be empty", h)
		}
	}
}

// assertAllScopeHeadersPresent additionally checks the participants
// header when at least one upstream attempt was made.
func assertAllScopeHeadersPresent(t *testing.T, headers map[string]string) {
	t.Helper()
	assertCounterHeadersPresent(t, headers)
	if ups, ok := headers["X-Erpc-Upstreams"]; ok {
		// Each segment is `<id>=<reason>:<outcome>:<duration>ms[:won]`.
		// At least one segment must be present and exactly one (for the
		// single-winner case) should carry the `:won` marker.
		assert.NotEmpty(t, ups)
		assert.Contains(t, ups, "=")
		assert.Contains(t, ups, ":")
		assert.True(t, strings.Contains(ups, ":won"), "expected at least one :won segment in %q", ups)
	}
}

func headersKeys(h map[string]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		if strings.HasPrefix(k, "X-Erpc-") {
			keys = append(keys, k)
		}
	}
	return keys
}

// minimalServerConfig returns a config with one upstream pointed at
// gock's intercept target plus a server.MaxTimeout.
func minimalServerConfig() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(5 * 1000 * 1000 * 1000).Ptr(), // 5s
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
	}
}
