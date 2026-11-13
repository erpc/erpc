package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// mockSvmMethod registers a persistent reply for `method` on host and returns a
// counter of how many times the upstream actually answered it.
func mockSvmMethod(host, method, body string) *atomic.Int64 {
	return mockSvmMethodDelayed(host, method, body, 0)
}

// mockSvmMethodDelayed is mockSvmMethod with each answer held for delay, so a
// network hedge fires while the attempt is in flight.
func mockSvmMethodDelayed(host, method, body string, delay time.Duration) *atomic.Int64 {
	var calls atomic.Int64
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == host &&
				strings.Contains(util.SafeReadBody(r), `"method":"`+method+`"`)
		}).
		Reply(200).
		Delay(delay).
		Map(func(res *http.Response) *http.Response {
			calls.Add(1)
			return res
		}).
		BodyString(body)
	return &calls
}

func svmSigPage(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"signature":"sig%d","slot":%d,"err":null}`, i, 100+i)
	}
	return `{"jsonrpc":"2.0","id":1,"result":[` + strings.Join(items, ",") + `]}`
}

// The archive's deeper known floor puts it first despite the pinned score order,
// so the full page arrives in one call and the pruned validator is never asked.
func TestSvm_GetSignaturesForAddress_DeepestFloorAnswersFirst(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigs-rpc1.localhost", 1000, 990, 1000, 500)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigs-rpc2.localhost", 1000, 990, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// rpc1 is a pruned validator (floor 500): its history for the address ends
	// after 2. rpc2 is the archive (floor 0).
	short := mockSvmMethod("svm-sigs-rpc1.localhost", "getSignaturesForAddress", svmSigPage(2))
	full := mockSvmMethod("svm-sigs-rpc2.localhost", "getSignaturesForAddress", svmSigPage(5))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigs-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigs-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")
	time.Sleep(300 * time.Millisecond) // let the floor poll land

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"sig4"`, "the full page from the archive must win")
	require.Zero(t, short.Load(), "the shallower upstream is not tried before the deepest one")
	require.EqualValues(t, 1, full.Load())
}

func TestSvm_GetSignaturesForAddress_FullPageStopsSweep(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePoller("svm-sigsfull-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-sigsfull-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := mockSvmMethod("svm-sigsfull-rpc1.localhost", "getSignaturesForAddress", svmSigPage(5))
	second := mockSvmMethod("svm-sigsfull-rpc2.localhost", "getSignaturesForAddress", svmSigPage(5))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigsfull-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigsfull-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int64(1), first.Load())
	require.Zero(t, second.Load(), "a full page must not trigger the sweep")
}

// Equal floors: a short page is the normal end of history, so the first
// upstream's answer is final and no other upstream is called.
func TestSvm_GetSignaturesForAddress_ShortPageAtEqualFloorsIsServed(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	hosts := []string{"svm-sigseq-rpc1.localhost", "svm-sigseq-rpc2.localhost", "svm-sigseq-rpc3.localhost"}
	for _, h := range hosts {
		util.SetupMocksForSvmStatePoller(h, 1000, 990)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make([]*atomic.Int64, len(hosts))
	cfgs := make([]*common.UpstreamConfig, len(hosts))
	for i, h := range hosts {
		calls[i] = mockSvmMethod(h, "getSignaturesForAddress", svmSigPage(2))
		cfgs[i] = svmUpstreamConfig(fmt.Sprintf("rpc%d", i+1), h)
	}
	net := setupTestSvmNetwork(t, ctx, cfgs)
	net.PinUpstreamOrderForTest("rpc1", "rpc2", "rpc3")
	time.Sleep(300 * time.Millisecond) // let the floor poll land

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"sig1"`)
	require.Equal(t, int64(1), calls[0].Load())
	require.Zero(t, calls[1].Load()+calls[2].Load(), "equal floors give no reason to call another upstream")
}

func TestSvm_GetSignaturesForAddress_AllShortServesLastValidPage(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigsall-rpc1.localhost", 1000, 990, 1000, 500)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigsall-rpc2.localhost", 1000, 990, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An address with only 2 signatures is legitimately short everywhere: the
	// client still gets the answer, not an error. rpc1 is pruned (floor 500) and
	// sweeps on; rpc2 holds the deepest history (floor 0), so its short page is
	// authoritative.
	mockSvmMethod("svm-sigsall-rpc1.localhost", "getSignaturesForAddress", svmSigPage(2))
	mockSvmMethod("svm-sigsall-rpc2.localhost", "getSignaturesForAddress", svmSigPage(2))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigsall-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigsall-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")
	time.Sleep(300 * time.Millisecond) // let the floor poll land

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr"]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"sig1"`)
}

const (
	svmSigStatusFound = `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":900},"value":[{"slot":5,"confirmations":null,"err":null,"confirmationStatus":"finalized"}]}}`
	svmSigStatusNull  = `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":900},"value":[null]}}`
)

func TestSvm_GetSignatureStatuses_SearchHistoryNullContinuesToNextUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-status-rpc1.localhost", 1000, 990, 1000, 500)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-status-rpc2.localhost", 1000, 990, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSvmMethod("svm-status-rpc1.localhost", "getSignatureStatuses", svmSigStatusNull)
	archive := mockSvmMethod("svm-status-rpc2.localhost", "getSignatureStatuses", svmSigStatusFound)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-status-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-status-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")
	time.Sleep(300 * time.Millisecond) // let the floor poll land

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses","params":[["sig"],{"searchTransactionHistory":true}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"finalized"`)
	require.GreaterOrEqual(t, archive.Load(), int64(1))
}

func TestSvm_GetSignatureStatuses_WithoutFlagNullIsServed(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePoller("svm-statusoff-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-statusoff-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSvmMethod("svm-statusoff-rpc1.localhost", "getSignatureStatuses", svmSigStatusNull)
	second := mockSvmMethod("svm-statusoff-rpc2.localhost", "getSignatureStatuses", svmSigStatusFound)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-statusoff-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-statusoff-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses","params":[["sig"]]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `[null]`, "a null is a valid recent-status answer")
	require.Zero(t, second.Load(), "no sweep without searchTransactionHistory")
}

// The shortest page must never beat the most complete one. The archive (floor 0)
// answers first with 4 of limit 5 (short because the address only has 4); the
// pruned validator (floor 500) would answer 2. The archive holds the pool's
// deepest history, so its short page is the answer and the sweep stops there.
func TestSvm_GetSignaturesForAddress_DeepestUpstreamShortPageIsAuthoritative(t *testing.T) {
	for _, order := range [][2]string{{"archive", "pruned"}, {"pruned", "archive"}} {
		order := order
		t.Run(order[0]+"-first", func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			archiveHost := "svm-sigsdeep-archive-" + order[0] + ".localhost"
			prunedHost := "svm-sigsdeep-pruned-" + order[0] + ".localhost"
			util.SetupMocksForSvmStatePollerWithFirstAvailable(archiveHost, 1000, 990, 1000, 0)
			util.SetupMocksForSvmStatePollerWithFirstAvailable(prunedHost, 1000, 990, 1000, 500)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			archive := mockSvmMethod(archiveHost, "getSignaturesForAddress", svmSigPage(4))
			pruned := mockSvmMethod(prunedHost, "getSignaturesForAddress", svmSigPage(2))

			ids := map[string]string{"archive": archiveHost, "pruned": prunedHost}
			net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
				svmUpstreamConfig(order[0], ids[order[0]]),
				svmUpstreamConfig(order[1], ids[order[1]]),
			})
			net.PinUpstreamOrderForTest(order[0], order[1])
			time.Sleep(300 * time.Millisecond) // let the floor poll land

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
			resp, err := svmProjectForward(ctx, net, req)
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			require.Contains(t, string(jrr.GetResultBytes()), `"sig3"`, "the archive's 4-entry page must be served")
			require.GreaterOrEqual(t, archive.Load(), int64(1))
			if order[0] == "archive" {
				require.Zero(t, pruned.Load(), "the deepest upstream's page ends the sweep")
			}
		})
	}
}

// Same for getSignatureStatuses: the archive's [found, null] (a signature that
// genuinely never landed) must not be replaced by the pruned node's [null, null].
func TestSvm_GetSignatureStatuses_DeepestUpstreamNullIsAuthoritative(t *testing.T) {
	const archiveBody = `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":900},"value":[{"slot":5,"confirmations":null,"err":null,"confirmationStatus":"finalized"},null]}}`
	const prunedBody = `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":900},"value":[null,null]}}`
	for _, order := range [][2]string{{"archive", "pruned"}, {"pruned", "archive"}} {
		order := order
		t.Run(order[0]+"-first", func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			archiveHost := "svm-statusdeep-archive-" + order[0] + ".localhost"
			prunedHost := "svm-statusdeep-pruned-" + order[0] + ".localhost"
			util.SetupMocksForSvmStatePollerWithFirstAvailable(archiveHost, 1000, 990, 1000, 0)
			util.SetupMocksForSvmStatePollerWithFirstAvailable(prunedHost, 1000, 990, 1000, 500)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			mockSvmMethod(archiveHost, "getSignatureStatuses", archiveBody)
			mockSvmMethod(prunedHost, "getSignatureStatuses", prunedBody)

			ids := map[string]string{"archive": archiveHost, "pruned": prunedHost}
			net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
				svmUpstreamConfig(order[0], ids[order[0]]),
				svmUpstreamConfig(order[1], ids[order[1]]),
			})
			net.PinUpstreamOrderForTest(order[0], order[1])
			time.Sleep(300 * time.Millisecond) // let the floor poll land

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses","params":[["a","b"],{"searchTransactionHistory":true}]}`))
			resp, err := svmProjectForward(ctx, net, req)
			require.NoError(t, err)
			jrr, err := resp.JsonRpcResponse()
			require.NoError(t, err)
			require.Contains(t, string(jrr.GetResultBytes()), `"finalized"`, "the archive's [found, null] must be served")
		})
	}
}

// When rpc1 (shallow) and rpc2 (deepest) return the same legitimately short page,
// rpc1's sweep-continuation MissingData must not record wrong-empty misbehavior.
func TestSvm_HistorySweep_NoMisbehaviorForIdenticalShortPages(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigsmb-rpc1.localhost", 1000, 990, 1000, 500)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-sigsmb-rpc2.localhost", 1000, 990, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSvmMethod("svm-sigsmb-rpc1.localhost", "getSignaturesForAddress", svmSigPage(2))
	mockSvmMethod("svm-sigsmb-rpc2.localhost", "getSignaturesForAddress", svmSigPage(2))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigsmb-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigsmb-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")
	time.Sleep(300 * time.Millisecond)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	var rpc1 *upstream.Upstream
	for _, u := range net.upstreamsRegistry.GetAllUpstreams() {
		if u.Id() == "rpc1" {
			rpc1 = u
			break
		}
	}
	require.NotNil(t, rpc1)
	metrics := net.metricsTracker.GetUpstreamMethodMetrics(rpc1, "getSignaturesForAddress", common.DataFinalityStateRealtime)
	require.Zero(t, metrics.MisbehaviorsTotal.Load(), "the history sweep must not punish an upstream that returned the same page as the winner")
}

// Consensus participants vote on the bodies they returned: the shallower
// participant's short page must stay a 200, or consensus agrees on an error.
func TestSvm_Consensus_AllShortSignaturePagesAgreeOnSuccess(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-cons-sigs-a.localhost", 1000, 990, 1000, 500)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-cons-sigs-b.localhost", 1000, 990, 1000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shortBody := svmSigPage(2)
	mockSvmMethod("svm-cons-sigs-a.localhost", "getSignaturesForAddress", shortBody)
	mockSvmMethod("svm-cons-sigs-b.localhost", "getSignaturesForAddress", shortBody)

	net := setupTestSvmNetworkWithConsensus(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("a", "svm-cons-sigs-a.localhost"),
		svmUpstreamConfig("b", "svm-cons-sigs-b.localhost"),
	}, []common.DataFinalityState{common.DataFinalityStateRealtime})
	time.Sleep(300 * time.Millisecond) // let the floor poll land

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err, "identical short pages must consensus-agree as success, not MissingData")
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"sig1"`)
}

// svmSweepNetwork mirrors a production Solana network: one hedge leg, three
// network attempts, and retryEmpty on, so a sweep that is not bounded per
// request shows up as repeat calls to the same upstream.
func svmSweepNetwork(n *common.NetworkConfig) {
	retry := func() *common.RetryPolicyConfig {
		return &common.RetryPolicyConfig{
			MaxAttempts:       3,
			Delay:             common.Duration(20 * time.Millisecond),
			EmptyResultAccept: []string{"getSignaturesForAddress"},
		}
	}
	hedge := func() *common.HedgePolicyConfig {
		return &common.HedgePolicyConfig{Delay: common.NewStaticDuration(150 * time.Millisecond), MaxCount: 1}
	}
	n.Failsafe = []*common.FailsafeConfig{
		{MatchMethod: "getSignaturesForAddress", Hedge: hedge(), Retry: retry()},
		{MatchMethod: "*", Hedge: hedge(), Retry: retry()},
	}
	n.DirectiveDefaults = &common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)}
}

// A permanent missing-data verdict (a skipped slot, a history page that ends
// at the upstream's floor) is swept across upstreams once per request: each
// upstream is asked once, by whichever leg or attempt reaches it first, and
// the request then ends with the verdict. A not-yet-available block keeps its
// wait-and-retry.
func TestSvm_PermanentMissingData_SweepsEachUpstreamOnce(t *testing.T) {
	const (
		skipped     = `{"jsonrpc":"2.0","id":1,"error":{"code":-32009,"message":"Slot 500 was skipped, or missing in long-term storage"}}`
		notYetThere = `{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Block not available for slot 500"}}`
	)
	for _, tc := range []struct {
		name     string
		method   string
		params   string
		bodies   []string // per-upstream body; nil = body for every upstream
		body     string
		floors   []int64 // first available slot per upstream; -1 = none observed
		tip      int64   // pool's processed slot, finalized 10 below; 0 = 1000
		ignoreAt int     // index of an upstream that ignores the method; -1 = none
		wantCode int     // client error code; 0 = a served result
		result   string  // served result when wantCode is 0; "" = []
		check    func(t *testing.T, per []int64, total int64)
	}{
		{
			name: "skipped slot", method: "getBlock", params: `[500]`, body: skipped,
			floors: []int64{-1, -1, -1, -1}, ignoreAt: -1, wantCode: -32009,
			check: func(t *testing.T, _ []int64, total int64) {
				require.Equal(t, int64(4), total, "each upstream is asked about a skipped slot once")
			},
		},
		{
			// An unknown `before` signature: one upstream rejects the cursor
			// (-32020), the others answer an empty page, and a fourth does not
			// serve the method. The rejection is that upstream's verdict, so
			// neither the hedge leg nor a retry asks it again, and the request
			// ends with the empty page the others gave.
			name: "unknown before signature", method: "getSignaturesForAddress", params: `["addr",{"limit":5,"before":"unknownsig"}]`,
			bodies: []string{
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"Transaction unknownsig not found"}}`,
				`{"jsonrpc":"2.0","id":1,"result":[]}`,
				`{"jsonrpc":"2.0","id":1,"result":[]}`,
				`{"jsonrpc":"2.0","id":1,"result":[]}`,
			},
			floors: []int64{0, 500, 400, -1}, ignoreAt: 3,
			check: func(t *testing.T, per []int64, _ int64) {
				require.Equal(t, []int64{1, 1, 1, 0}, per, "each serving upstream is asked for the page once")
			},
		},
		{
			name: "block not yet available", method: "getBlock", params: `[500]`, body: notYetThere,
			floors: []int64{-1, -1, -1, -1}, ignoreAt: -1, wantCode: -32004,
			check: func(t *testing.T, _ []int64, total int64) {
				require.Greater(t, total, int64(4), "a not-yet-available block is retried after a delay")
			},
		},
		{
			// One provider answers null for a skipped slot where the others
			// answer agave's skip errors. The null is that provider's missing
			// data, not the request's answer, and nobody is asked twice.
			name: "getBlockTime null for a skipped slot", method: "getBlockTime", params: `[400000000]`, tip: 400_001_000,
			bodies: []string{
				`{"jsonrpc":"2.0","id":1,"result":null}`,
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32009,"message":"Slot 400000000 was skipped, or missing in long-term storage"}}`,
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32007,"message":"Slot 400000000 was skipped, or missing due to ledger jump to recent snapshot"}}`,
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32009,"message":"Slot 400000000 was skipped, or missing in long-term storage"}}`,
			},
			floors: []int64{-1, -1, -1, -1}, ignoreAt: -1, wantCode: -32009,
			check: func(t *testing.T, per []int64, _ int64) {
				require.Equal(t, []int64{1, 1, 1, 1}, per, "each upstream is asked about a skipped slot once")
			},
		},
		{
			// Before mainnet-beta recorded block times, null is agave's answer
			// and reaches the client.
			name: "getBlockTime null before recorded times", method: "getBlockTime", params: `[500]`,
			body:   `{"jsonrpc":"2.0","id":1,"result":null}`,
			floors: []int64{-1, -1, -1, -1}, ignoreAt: -1, result: "null",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ids := []string{"rpc1", "rpc2", "rpc3", "rpc4"}
			calls := make([]*atomic.Int64, len(ids))
			cfgs := make([]*common.UpstreamConfig, len(ids))
			for i, id := range ids {
				host := fmt.Sprintf("svm-sweeponce-%d-%s.localhost", len(tc.name), id)
				tip := tc.tip
				if tip == 0 {
					tip = 1000
				}
				if tc.floors[i] < 0 {
					util.SetupMocksForSvmStatePoller(host, tip, tip-10)
				} else {
					util.SetupMocksForSvmStatePollerWithFirstAvailable(host, tip, tip-10, tip, tc.floors[i])
				}
				body := tc.body
				if tc.bodies != nil {
					body = tc.bodies[i]
				}
				calls[i] = mockSvmMethodDelayed(host, tc.method, body, 100*time.Millisecond)
				cfgs[i] = svmUpstreamConfig(id, host)
				if i == tc.ignoreAt {
					cfgs[i].IgnoreMethods = []string{tc.method}
				}
			}
			net := setupTestSvmNetworkWithConfig(t, ctx, svmSweepNetwork, cfgs)
			net.PinUpstreamOrderForTest(ids...)
			time.Sleep(300 * time.Millisecond) // let the floor poll land

			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + tc.method + `","params":` + tc.params + `}`))
			resp, err := svmProjectForward(ctx, net, req)

			var total int64
			per := make([]int64, len(calls))
			for i, c := range calls {
				per[i] = c.Load()
				total += per[i]
			}
			t.Logf("upstream calls per upstream: %v (total %d), exec %+v", per, total, req.ExecState().Snapshot())
			if tc.wantCode == 0 {
				require.NoError(t, err)
				require.NotNil(t, resp)
				jrr, jerr := resp.JsonRpcResponse()
				require.NoError(t, jerr)
				want := tc.result
				if want == "" {
					want = "[]"
				}
				require.Equal(t, want, string(jrr.GetResultBytes()))
			} else {
				require.Error(t, err)
				require.Equal(t, common.JsonRpcErrorNumber(tc.wantCode), svmClientErrorCode(t, req, err))
			}
			if tc.check != nil {
				tc.check(t, per, total)
			}
		})
	}
}
