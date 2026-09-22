package erpc

import (
	"bytes"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// serverSideCause mirrors the plain 5xx upstream failure that
// architecture/evm/error_normalizer.go emits for an unrecognised provider
// error: retryable toward the network, and carrying no transport status of its
// own (determineResponseStatusCode maps it to 200).
func serverSideCause(message string) error {
	return common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(
			-32603,
			common.JsonRpcErrorServerSideException,
			message,
			nil,
			nil,
		),
		nil,
		500,
	)
}

// opStackSenderRateLimitCause reproduces the OP-Stack sequencer branch of
// architecture/evm/error_normalizer.go verbatim: a 429-bearing capacity error
// explicitly marked non-retryable toward the network, because every provider
// fronts the same sequencer and failing over is futile.
//
// That marker is what makes this the hardest cause to preserve: orderCauses
// ranks retryable causes first, so this one sorts LAST behind every plain 5xx,
// and TranslateToJsonRpcException's keep-one dominance scan can never elect it.
// If HTTP status were derived from the pruned error, a client hitting the
// sequencer's per-sender limit would be told 200 and resubmit the transaction
// instead of backing off.
func opStackSenderRateLimitCause(t *testing.T) error {
	t.Helper()
	capErr := common.NewErrEndpointCapacityExceeded(
		common.NewErrJsonRpcExceptionInternal(
			-32005,
			common.JsonRpcErrorCapacityExceeded,
			"sender is over rate limit",
			nil,
			nil,
		),
	)
	re, ok := capErr.(common.RetryableError)
	require.Truef(t, ok, "capacity error must be markable non-retryable, got %T", capErr)
	return re.WithRetryableTowardNetwork(false)
}

// unauthorizedCause is a per-upstream 401 (bad/expired provider key), which
// determineResponseStatusCode maps to http.StatusUnauthorized.
func unauthorizedCause() error {
	return common.NewErrEndpointUnauthorized(
		common.NewErrJsonRpcExceptionInternal(
			-32016,
			common.JsonRpcErrorUnauthorized,
			"invalid api key",
			nil,
			nil,
		),
	)
}

// wrappedExhaustedBundle assembles the exact error shape the network retry loop
// hands the HTTP layer: per-upstream causes -> ErrUpstreamsExhausted ->
// ErrFailsafeRetryExceeded.
//
// The ErrFailsafeRetryExceeded wrapper is load-bearing, not decoration. Without
// it buildErrorResponseBody's single-cause shortcut unwraps the bundle before
// translation, and findUpstreamsExhausted is precisely the hop that makes the
// prune reachable through it — so a bundle built without the wrapper would not
// exercise the code path under test at all.
func wrappedExhaustedBundle(causes map[string]error) error {
	m := &sync.Map{}
	for id, cause := range causes {
		m.Store(id, cause)
	}
	exhausted := common.NewErrUpstreamsExhausted(
		nil, m, "prj", "evm:10", "eth_sendRawTransaction",
		time.Second, len(causes), len(causes)-1, 0, len(causes),
	)
	return common.NewErrFailsafeRetryExceeded(common.ScopeNetwork, exhausted, nil)
}

func newSendRawTxRequest(t *testing.T) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":7,"method":"eth_sendRawTransaction","params":["0xdeadbeef"]}`,
	))
	req.SetNetwork(&Network{networkId: "evm:10"})
	return req
}

// TranslateToJsonRpcException collapses an exhausted bundle to a single
// dominant cause so the client gets a readable message. That prune keeps
// exactly ONE cause, so any status-bearing sibling (429 capacity, 401
// unauthorized) sitting next to plain 5xx failures used to be discarded and the
// response degraded to HTTP 200.
//
// Reordering cannot fix this: when a 429 and a 401 compete, a keep-one prune
// can preserve at most one of them. SURVIVAL of the transport status is
// therefore the property under test — HTTP status must be derived from the
// whole unpruned cause tree.
func TestDetermineResponseStatusCode_ExhaustedBundlePrunePreservesStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		causes    map[string]error
		wantOneOf []int
		why       string
	}{
		{
			name: "op-stack sender rate limit survives two 5xx siblings",
			causes: map[string]error{
				"up-a": serverSideCause("upstream a exploded"),
				"up-b": serverSideCause("upstream b exploded"),
				"up-c": opStackSenderRateLimitCause(t),
			},
			wantOneOf: []int{http.StatusTooManyRequests},
			why:       "429 is the only backpressure signal; losing it makes clients resubmit the tx",
		},
		{
			name: "endpoint unauthorized survives two 5xx siblings",
			causes: map[string]error{
				"up-a": serverSideCause("upstream a exploded"),
				"up-b": serverSideCause("upstream b exploded"),
				"up-c": unauthorizedCause(),
			},
			wantOneOf: []int{http.StatusUnauthorized},
			why:       "401 must reach the client so a bad provider key is actionable",
		},
		{
			name: "429 and 401 competing with a 5xx: one of them must survive",
			causes: map[string]error{
				"up-a": serverSideCause("upstream a exploded"),
				"up-b": unauthorizedCause(),
				"up-c": opStackSenderRateLimitCause(t),
			},
			// Two transport statuses compete and a keep-one prune can carry at
			// most one, so no ordering of causes can satisfy this row. It is
			// only satisfiable by reading status from the unpruned tree.
			wantOneOf: []int{http.StatusUnauthorized, http.StatusTooManyRequests},
			why:       "structurally unsatisfiable by reordering; only an unpruned status read passes",
		},
		{
			name: "control: an all-5xx bundle still reports 200",
			causes: map[string]error{
				"up-a": serverSideCause("upstream a exploded"),
				"up-b": serverSideCause("upstream b exploded"),
				"up-c": serverSideCause("upstream c exploded"),
			},
			// Passes both before and after the fix. Without this row the suite
			// would only be asserting "anything but 200".
			wantOneOf: []int{http.StatusOK},
			why:       "JSON-RPC application errors keep 200; the fix must not blanket-escalate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// sync.Map.Range randomizes independently of insertion order, so
			// replay: the status must be a pure function of the multiset of
			// causes, never of whichever one the prune happens to elect.
			for i := range 20 {
				bundle := wrappedExhaustedBundle(tc.causes)
				body := buildErrorResponseBody(newSendRawTxRequest(t), bundle, bundle, nil)
				require.Containsf(t, tc.wantOneOf, determineResponseStatusCode(body),
					"iteration %d: %s", i, tc.why)
			}
		})
	}
}

// The status fix must be invisible on the wire: the JSON-RPC error object still
// reports the PRUNED dominant cause (the readable 5xx that two upstreams
// agreed on), while the HTTP status reflects the whole tree. Cause is
// `json:"-"`, so widening it must not add anything client-visible.
func TestBuildErrorResponseBody_PrunedBodyKeepsDominantCauseWhileStatusKeepsBundle(t *testing.T) {
	causes := map[string]error{
		"up-a": serverSideCause("upstream a exploded"),
		"up-b": serverSideCause("upstream b exploded"),
		"up-c": opStackSenderRateLimitCause(t),
	}

	for i := range 20 {
		bundle := wrappedExhaustedBundle(causes)
		body := buildErrorResponseBody(newSendRawTxRequest(t), bundle, bundle, nil)

		response, ok := body.(*HttpJsonRpcErrorResponse)
		require.Truef(t, ok, "iteration %d: expected HttpJsonRpcErrorResponse, got %T", i, body)

		errObject, ok := response.Error.(map[string]interface{})
		require.Truef(t, ok, "iteration %d: expected JSON-RPC error object, got %T", i, response.Error)

		// Body: the dominant cause. ErrEndpointServerSideException occurs twice
		// so it wins the tally, and orderCauses breaks the tie by upstream id,
		// making "up-a" the representative on every run.
		require.EqualValuesf(t, common.JsonRpcErrorServerSideException, errObject["code"],
			"iteration %d: body must keep the pruned dominant cause's wire code", i)
		require.Equalf(t, "upstream a exploded", errObject["message"],
			"iteration %d: body must keep the pruned dominant cause's message", i)

		// Status: the whole tree, including the capacity sibling the prune dropped.
		require.Equalf(t, http.StatusTooManyRequests, determineResponseStatusCode(response),
			"iteration %d: status must be derived from the unpruned cause tree", i)

		// Wire contract: only the pruned view is serialized. The widened Cause
		// (which carries every upstream's error, endpoint URLs included) must
		// never reach the client.
		var buf bytes.Buffer
		_, err := writeJsonRpcError(&buf, response)
		require.NoErrorf(t, err, "iteration %d", i)

		var payload map[string]interface{}
		require.NoErrorf(t, common.SonicCfg.Unmarshal(buf.Bytes(), &payload), "iteration %d: %s", i, buf.String())

		keys := make([]string, 0, len(payload))
		for k := range payload {
			keys = append(keys, k)
		}
		require.ElementsMatchf(t, []string{"jsonrpc", "id", "error"}, keys,
			"iteration %d: unexpected client-visible fields in %s", i, buf.String())

		wireError, ok := payload["error"].(map[string]interface{})
		require.Truef(t, ok, "iteration %d: expected serialized error object, got %T", i, payload["error"])
		require.EqualValuesf(t, common.JsonRpcErrorServerSideException, wireError["code"],
			"iteration %d: serialized wire code changed", i)
		require.Equalf(t, "upstream a exploded", wireError["message"],
			"iteration %d: serialized wire message changed", i)
	}
}

// A network no provider serves must not reach the client as a plain JSON-RPC
// application error on HTTP 200. It is a coverage gap rather than a server
// fault, and it is terminal — 404 names it honestly, stops a retry loop that
// cannot succeed, and still engages client-side failover in multi-provider
// setups. The body must name the condition, not promise that a retry helps.
func TestDetermineResponseStatusCode_NoUpstreamsAvailableIs404(t *testing.T) {
	err := common.NewErrNetworkNoUpstreamsAvailable("prjA", "evm:534351")

	require.Equal(t, http.StatusNotFound, determineResponseStatusCode(err))

	body := buildErrorResponseBody(nil, err, err, nil)
	require.Equal(t, http.StatusNotFound, determineResponseStatusCode(body))

	encoded, jerr := common.SonicCfg.Marshal(body)
	require.NoError(t, jerr)
	require.Contains(t, string(encoded), "no RPC providers are available for network 'evm:534351'")
	require.NotContains(t, string(encoded), "retry shortly")
}
