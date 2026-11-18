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

// mockSvmBody registers a persistent reply on host for requests whose body
// contains every needle, and counts the answers.
func mockSvmBody(host, body string, needles ...string) *atomic.Int64 {
	var calls atomic.Int64
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != host {
				return false
			}
			b := util.SafeReadBody(r)
			for _, n := range needles {
				if !strings.Contains(b, n) {
					return false
				}
			}
			return true
		}).
		Reply(200).
		Map(func(res *http.Response) *http.Response {
			calls.Add(1)
			return res
		}).
		BodyString(body)
	return &calls
}

// With svm.probeSkippedSlots, a slot the first upstream proves skipped from its
// own finalized ledger (the next finalized block's parent is below it) is
// answered without asking the other upstreams or escalating to the fallback
// tier. Anything short of a proof, or the probe off, sweeps every upstream
// once, as before.
func TestSvm_SkippedSlot_ProvenByNextBlockParentEndsSweep(t *testing.T) {
	const (
		ledgerJump  = `{"jsonrpc":"2.0","id":1,"error":{"code":-32007,"message":"Slot 500 was skipped, or missing due to ledger jump to recent snapshot"}}`
		longTerm    = `{"jsonrpc":"2.0","id":1,"error":{"code":-32009,"message":"Slot 500 was skipped, or missing in long-term storage"}}`
		notYetThere = `{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Block not available for slot 500"}}`
		next502     = `{"jsonrpc":"2.0","id":1,"result":[502]}`
		noNext      = `{"jsonrpc":"2.0","id":1,"result":[]}`
	)
	header := func(parent int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"blockHeight":400,"blockTime":1700000000,"blockhash":"h502","parentSlot":%d,"previousBlockhash":"h%d"}}`, parent, parent)
	}
	for _, tc := range []struct {
		name      string
		answer    string // getBlock(500) on every upstream
		next      string // rpc1's getBlocksWithLimit(501, 1); "" = not mocked (the read fails)
		header    string // rpc1's getBlock(502); "" = not mocked
		finalized int64  // every poller's finalized slot
		commit    string // the request's commitment; "" = finalized
		probeOff  bool   // svm.probeSkippedSlots unset
		wantCode  int
		wantAsked []int64 // getBlock(500) calls per upstream, fallback last
		wantProbe bool    // rpc1 was asked for the next block
	}{
		{name: "proven skipped (-32007)", answer: ledgerJump, next: next502, header: header(499), finalized: 990,
			wantCode: -32007, wantAsked: []int64{1, 0, 0, 0}, wantProbe: true},
		{name: "proven skipped (-32009)", answer: longTerm, next: next502, header: header(499), finalized: 990,
			wantCode: -32009, wantAsked: []int64{1, 0, 0, 0}, wantProbe: true},
		{name: "next block's parent is the slot itself", answer: ledgerJump, next: next502, header: header(500), finalized: 990,
			wantCode: -32007, wantAsked: []int64{1, 1, 1, 1}, wantProbe: true},
		{name: "no finalized block after the slot", answer: ledgerJump, next: noNext, finalized: 990,
			wantCode: -32007, wantAsked: []int64{1, 1, 1, 1}, wantProbe: true},
		{name: "next-block read fails", answer: ledgerJump, finalized: 990,
			wantCode: -32007, wantAsked: []int64{1, 1, 1, 1}},
		{name: "upstream not finalized past the slot", answer: ledgerJump, next: next502, header: header(499), finalized: 495, commit: "confirmed",
			wantCode: -32007, wantAsked: []int64{1, 1, 1, 1}},
		{name: "probe off (default)", answer: ledgerJump, next: next502, header: header(499), finalized: 990, probeOff: true,
			wantCode: -32007, wantAsked: []int64{1, 1, 1, 1}},
		{name: "not-yet-available is not a skip verdict", answer: notYetThere, next: next502, header: header(499), finalized: 990,
			wantCode: -32004},
	} {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			t.Cleanup(util.ResetGock)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ids := []string{"rpc1", "rpc2", "rpc3", "fallback"}
			asked := make([]*atomic.Int64, len(ids))
			cfgs := make([]*common.UpstreamConfig, len(ids))
			var probed *atomic.Int64
			for i, id := range ids {
				host := fmt.Sprintf("svm-skipproof-%d-%s.localhost", len(tc.name), id)
				util.SetupMocksForSvmStatePoller(host, 1000, tc.finalized)
				if i == 0 {
					if tc.next != "" {
						probed = mockSvmBody(host, tc.next, `"method":"getBlocksWithLimit"`, `"params":[501,1`)
					}
					if tc.header != "" {
						mockSvmBody(host, tc.header, `"method":"getBlock"`, `"params":[502`)
					}
				}
				asked[i] = mockSvmBody(host, tc.answer, `"method":"getBlock"`, `"params":[500`)
				cfgs[i] = svmUpstreamConfig(id, host)
			}
			cfgs[len(cfgs)-1].Tags = []string{"tier:fallback"}
			net := setupTestSvmNetworkWithConfig(t, ctx, func(n *common.NetworkConfig) {
				svmSweepNetwork(n)
				n.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
				if !tc.probeOff {
					n.Svm.ProbeSkippedSlots = util.BoolPtr(true)
				}
			}, cfgs)
			net.PinUpstreamOrderForTest(ids...)
			time.Sleep(300 * time.Millisecond) // let the finalized slot poll land

			commit := tc.commit
			if commit == "" {
				commit = "finalized"
			}
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[500,{"commitment":"` + commit + `"}]}`))
			_, err := svmProjectForward(ctx, net, req)

			per := make([]int64, len(asked))
			for i, c := range asked {
				per[i] = c.Load()
			}
			t.Logf("getBlock(500) calls per upstream: %v", per)
			require.Error(t, err)
			require.Equal(t, common.JsonRpcErrorNumber(tc.wantCode), svmClientErrorCode(t, req, err))
			if tc.wantAsked != nil {
				require.Equal(t, tc.wantAsked, per)
			}
			if probed != nil {
				require.Equal(t, tc.wantProbe, probed.Load() > 0, "next-block read on rpc1")
			}
		})
	}
}
