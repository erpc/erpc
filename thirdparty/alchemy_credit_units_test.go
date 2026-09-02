package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Keep unit tests hermetic: point the CU docs fetch at an address that
	// refuses instantly, so the lifecycle refresh triggered by SupportsNetwork
	// / GenerateConfigs never reaches the real docs endpoint. Tests that
	// exercise fetching swap this per-test to a mock server.
	alchemyCreditUnitsURL = "http://127.0.0.1:1/disabled-in-tests"
}

// swapAlchemyCreditUnitsURL temporarily overrides the package-level
// alchemyCreditUnitsURL so tests can point the CU fetch at a mock server.
func swapAlchemyCreditUnitsURL(t *testing.T, newURL string) string {
	t.Helper()
	prev := alchemyCreditUnitsURL
	alchemyCreditUnitsURL = newURL
	return prev
}

func cuReq(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`,
	))
}

// A representative slice of the compute-unit-costs Markdown: two allowlisted
// EVM sections, one ignored (Solana) section, plus header/separator/blank-CU
// rows and a backticked method name.
const alchemyCUMarkdownSample = `# EVM: Standard JSON-RPC Methods

| Method | CU | Throughput CU |
| --- | --- | --- |
| eth_chainId | 0 | 5 |
| eth_call | 99 | |
| eth_getLogs | 60 | |
| ` + "`eth_getBalance`" + ` | 20 | |
| eth_subscribe |  | 10 |

# Solana: Standard JSON-RPC Methods

| Method | CU | Throughput CU |
| getAccountInfo | 10 | |

# Debug API

| Method | CU | Throughput CU |
| debug_traceTransaction | 40 | |
`

func TestParseAlchemyCreditUnits(t *testing.T) {
	out := parseAlchemyCreditUnits(alchemyCUMarkdownSample)

	assert.Equal(t, int64(0), out["eth_chainId"], "0-CU method parsed")
	assert.Equal(t, int64(99), out["eth_call"])
	assert.Equal(t, int64(60), out["eth_getLogs"])
	assert.Equal(t, int64(20), out["eth_getBalance"], "backticks stripped")
	assert.Equal(t, int64(40), out["debug_traceTransaction"], "Debug API section is allowlisted")

	_, hasBlank := out["eth_subscribe"]
	assert.False(t, hasBlank, "blank/non-numeric CU cell is skipped")
	_, hasSolana := out["getAccountInfo"]
	assert.False(t, hasSolana, "non-allowlisted (Solana) section is ignored")
	_, hasHeader := out["Method"]
	assert.False(t, hasHeader, "table header row is skipped")
	assert.Len(t, out, 5)
}

// The fetcher parses the live table and merges it over the built-in map:
// fetched values win, and built-in-only entries (incl. "*") survive.
func TestFetchAlchemyCreditUnits_MergesOverBuiltin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(alchemyCUMarkdownSample))
	}))
	defer server.Close()
	orig := swapAlchemyCreditUnitsURL(t, server.URL)
	defer swapAlchemyCreditUnitsURL(t, orig)

	table, err := fetchAlchemyCreditUnits(context.Background())
	require.NoError(t, err)

	assert.Equal(t, int64(99), table["eth_call"], "fetched value wins over built-in 26")
	assert.Equal(t, int64(60), table["eth_getLogs"])
	assert.Equal(t, int64(20), table["*"], "built-in '*' fallback survives the merge")
	assert.Equal(t, int64(40), table["eth_sendRawTransaction"], "built-in-only method (not in sample) survives")
}

func TestFetchAlchemyCreditUnits_Errors(t *testing.T) {
	// Non-2xx → error (cache keeps prior/built-in).
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	orig := swapAlchemyCreditUnitsURL(t, srv500.URL)
	_, err := fetchAlchemyCreditUnits(context.Background())
	assert.Error(t, err, "non-2xx surfaces an error")

	// 200 but nothing parseable → error (don't publish an empty table).
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# Unrelated\n\nno tables here\n"))
	}))
	defer srvEmpty.Close()
	swapAlchemyCreditUnitsURL(t, srvEmpty.URL)
	_, err = fetchAlchemyCreditUnits(context.Background())
	assert.Error(t, err, "no methods parsed surfaces an error")

	swapAlchemyCreditUnitsURL(t, orig)
}

// Before any fetch completes (cold cache), CreditUnits transparently uses the
// built-in table — even when the endpoint is unreachable.
func TestAlchemyCreditUnits_ColdStartFallback(t *testing.T) {
	orig := swapAlchemyCreditUnitsURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapAlchemyCreditUnitsURL(t, orig)

	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	assert.Equal(t, int64(26), vendor.CreditUnits(cuReq(t, "eth_call"), nil), "built-in eth_call cost")
	assert.Equal(t, int64(60), vendor.CreditUnits(cuReq(t, "eth_getLogs"), nil))
	assert.Equal(t, int64(20), vendor.CreditUnits(cuReq(t, "erpc_unlisted"), nil), "built-in '*' fallback")
}

// After the async refresh publishes, CreditUnits reflects the fetched table.
func TestAlchemyCreditUnits_FetchPromotesOverFallback(t *testing.T) {
	fetched := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(alchemyCUMarkdownSample))
		select {
		case fetched <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	orig := swapAlchemyCreditUnitsURL(t, server.URL)
	defer swapAlchemyCreditUnitsURL(t, orig)

	vendor := CreateAlchemyVendor().(*AlchemyVendor)

	// Cold cache: CreditUnits returns the built-in value and never fetches
	// (the hot path is a pure read).
	assert.Equal(t, int64(26), vendor.CreditUnits(cuReq(t, "eth_call"), nil))

	// The lifecycle trigger (as SupportsNetwork / GenerateConfigs would) kicks
	// off the async refresh.
	logger := zerolog.Nop()
	vendor.refreshCreditUnitsAsync(&logger)

	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("async CU refresh never hit the mock server")
	}

	// Once the snapshot is published, the fetched value (99) is used, and the
	// built-in "*" fallback still applies to unlisted methods.
	require.Eventually(t, func() bool {
		return vendor.CreditUnits(cuReq(t, "eth_call"), nil) == 99
	}, 5*time.Second, 50*time.Millisecond, "fetched CU table should promote over built-in")
	assert.Equal(t, int64(20), vendor.CreditUnits(cuReq(t, "erpc_unlisted"), nil))

	// Operator override still wins over the fetched table.
	over := &common.UpstreamConfig{CreditUnits: map[string]int64{"eth_call": 5}}
	assert.Equal(t, int64(5), vendor.CreditUnits(cuReq(t, "eth_call"), over))
}
