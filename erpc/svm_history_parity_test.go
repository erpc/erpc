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
	var calls atomic.Int64
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return r.URL.Host == host &&
				strings.Contains(util.SafeReadBody(r), `"method":"`+method+`"`)
		}).
		Reply(200).
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

func TestSvm_GetSignaturesForAddress_ShortPageContinuesToNextUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-sigs-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-sigs-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// rpc1 is a pruned validator: its history for the address ends after 2.
	short := mockSvmMethod("svm-sigs-rpc1.localhost", "getSignaturesForAddress", svmSigPage(2))
	full := mockSvmMethod("svm-sigs-rpc2.localhost", "getSignaturesForAddress", svmSigPage(5))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigs-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigs-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["addr",{"limit":5}]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Contains(t, string(jrr.GetResultBytes()), `"sig4"`, "the full page from the archive must win")
	require.GreaterOrEqual(t, short.Load(), int64(1))
	require.GreaterOrEqual(t, full.Load(), int64(1))
}

func TestSvm_GetSignaturesForAddress_FullPageStopsSweep(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
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

func TestSvm_GetSignaturesForAddress_AllShortServesLastValidPage(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-sigsall-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-sigsall-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An address with only 2 signatures is legitimately short everywhere: the
	// client still gets the answer, not an error.
	mockSvmMethod("svm-sigsall-rpc1.localhost", "getSignaturesForAddress", svmSigPage(2))
	mockSvmMethod("svm-sigsall-rpc2.localhost", "getSignaturesForAddress", svmSigPage(2))

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-sigsall-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-sigsall-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

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
	defer util.ResetGock()
	util.SetupMocksForSvmStatePoller("svm-status-rpc1.localhost", 1000, 990)
	util.SetupMocksForSvmStatePoller("svm-status-rpc2.localhost", 1000, 990)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockSvmMethod("svm-status-rpc1.localhost", "getSignatureStatuses", svmSigStatusNull)
	archive := mockSvmMethod("svm-status-rpc2.localhost", "getSignatureStatuses", svmSigStatusFound)

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-status-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-status-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")

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
	defer util.ResetGock()
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

func TestSvm_GetFirstAvailableBlock_PoolAnswersMinFloor(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// Pruned validator at 200000 next to an archive at genesis.
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-floor-rpc1.localhost", 300000, 299900, 300000, 200000)
	util.SetupMocksForSvmStatePollerWithFirstAvailable("svm-floor-rpc2.localhost", 300000, 299900, 300000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("rpc1", "svm-floor-rpc1.localhost"),
		svmUpstreamConfig("rpc2", "svm-floor-rpc2.localhost"),
	})
	net.PinUpstreamOrderForTest("rpc1", "rpc2")
	time.Sleep(300 * time.Millisecond)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getFirstAvailableBlock","params":[]}`))
	resp, err := svmProjectForward(ctx, net, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, "0", string(jrr.GetResultBytes()),
		"the pool floor is the archive's, not whichever upstream was picked first")
}
