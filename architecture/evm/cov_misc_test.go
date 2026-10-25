package evm

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ---------------------------------------------------------------------------
// multicall3
// ---------------------------------------------------------------------------

// covMiscMcUpstream is an upstream carrying a Multicall3 batcher and a
// pluggable Forward that receives the aggregate3 eth_call.
type covMiscMcUpstream struct {
	common.Upstream
	b       *Multicall3Batcher
	forward func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (u *covMiscMcUpstream) EvmMulticall3Batcher() *Multicall3Batcher { return u.b }
func (u *covMiscMcUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, _ bool, _ bool) (*common.NormalizedResponse, error) {
	return u.forward(ctx, nq)
}

func covMiscMcNew(t *testing.T, maxCalls int, maxWait time.Duration, forward func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error)) *covMiscMcUpstream {
	id := "mc-" + strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	return &covMiscMcUpstream{
		Upstream: common.NewFakeUpstream(id),
		b:        NewMulticall3Batcher(&common.EvmMulticall3Config{MaxCalls: maxCalls, MaxWait: common.Duration(maxWait)}),
		forward:  forward,
	}
}

func covMiscMcCount(u *covMiscMcUpstream, outcome string) float64 {
	return testutil.ToFloat64(telemetry.MetricUpstreamMulticall3CallsTotal.WithLabelValues("test-project", "n/a", u.Id(), u.VendorName(), outcome))
}

func covMiscMcCall(id int, to, data, block string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"%s","data":"%s"},"%s"]}`, id, to, data, block)))
}

func covMiscWord(v uint64) []byte {
	var w [32]byte
	binary.BigEndian.PutUint64(w[24:], v)
	return w[:]
}

// covMiscAgg3 ABI-encodes an aggregate3 return value: (bool, bytes)[].
func covMiscAgg3(results []aggregate3Result) []byte {
	out := append(covMiscWord(32), covMiscWord(uint64(len(results)))...)
	offset := uint64(32 * len(results))
	for _, r := range results {
		out = append(out, covMiscWord(offset)...)
		offset += uint64(96 + padded32(len(r.data)))
	}
	for _, r := range results {
		s := uint64(0)
		if r.success {
			s = 1
		}
		out = append(out, covMiscWord(s)...)
		out = append(out, covMiscWord(64)...)
		out = append(out, covMiscWord(uint64(len(r.data)))...)
		out = append(out, r.data...)
		out = append(out, make([]byte, padded32(len(r.data))-len(r.data))...)
	}
	return out
}

// covMiscAgg3Targets reads the inner-call targets back out of an aggregate3
// eth_call the batcher sent.
func covMiscAgg3Targets(t *testing.T, nq *common.NormalizedRequest) (to string, targets [][]byte) {
	jrq, err := nq.JsonRpcRequest()
	require.NoError(t, err)
	call := jrq.Params[0].(map[string]interface{})
	raw, err := hex.DecodeString(strings.TrimPrefix(call["data"].(string), "0x"))
	require.NoError(t, err)
	require.Equal(t, aggregate3Selector, raw[:4])
	body := raw[4:]
	count := int(binary.BigEndian.Uint64(body[56:64]))
	head := 64
	for i := range count {
		off := int(binary.BigEndian.Uint64(body[head+32*i+24 : head+32*i+32]))
		tuple := head + off
		targets = append(targets, body[tuple+12:tuple+32])
	}
	return call["to"].(string), targets
}

func covMiscMcResult(result string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil))
}

type covMiscMcOut struct {
	handled bool
	resp    *common.NormalizedResponse
	err     error
}

// covMiscMcRun submits every request concurrently and returns the hook's
// answers in request order.
func covMiscMcRun(ctxs []context.Context, n common.Network, u *covMiscMcUpstream, reqs []*common.NormalizedRequest) []covMiscMcOut {
	outs := make([]covMiscMcOut, len(reqs))
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, r, e := upstreamPreForward_eth_call(ctxs[i], n, u, reqs[i])
			outs[i] = covMiscMcOut{h, r, e}
		}(i)
	}
	wg.Wait()
	return outs
}

func covMiscBg(k int) []context.Context {
	out := make([]context.Context, k)
	for i := range out {
		out[i] = context.Background()
	}
	return out
}

func covMiscResultString(t *testing.T, r *common.NormalizedResponse) string {
	jrr, err := r.JsonRpcResponse()
	require.NoError(t, err)
	var s string
	require.NoError(t, json.Unmarshal(jrr.GetResultBytes(), &s))
	return s
}

// covMiscTagPack answers a tag pack: results for the clients' entries, then
// the block-number entry the batcher adds.
func covMiscTagPack(results ...aggregate3Result) []aggregate3Result {
	return append(results, aggregate3Result{success: true, data: covMiscWord(1)})
}

func TestCovMisc_Multicall3_PacksFullBatchAndAnswersEachCaller(t *testing.T) {
	const latest, executed = 100, 90
	n := &queryTestNetwork{latest: latest}
	tracker := newChainTracker(t.Context(), nil, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), nil))
	startStateProber(t.Context(), n, tracker, nil)
	p := stateProberFor(n)
	require.NotNil(t, p)
	t.Cleanup(func() { stateProbers.CompareAndDelete(n, p) })
	probeTarget, err := common.HexToBytes(integrity.Multicall3Address)
	require.NoError(t, err)

	addrA, addrB := covLogsAddr("a"), covLogsAddr("b")
	var gotDeadline bool
	var packed [][][]byte
	u := covMiscMcNew(t, 2, time.Minute, nil)
	u.forward = func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		_, gotDeadline = ctx.Deadline()
		assert.Equal(t, common.CompositeTypeMulticall3, nq.CompositeType())
		to, targets := covMiscAgg3Targets(t, nq)
		assert.Equal(t, integrity.Multicall3Address, to)
		packed = append(packed, targets)
		res := make([]aggregate3Result, len(targets))
		for i, tg := range targets {
			res[i] = aggregate3Result{success: true, data: tg}
			if string(tg) == string(probeTarget) {
				res[i].data = covMiscWord(executed)
			}
		}
		return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3(res)) + `"`), nil
	}
	before := covMiscMcCount(u, multicall3Packed)

	for _, block := range []string{"0x10", "latest"} {
		reqs := []*common.NormalizedRequest{covMiscMcCall(1, addrA, "0x01", block), covMiscMcCall(2, addrB, "0x02", block)}
		outs := covMiscMcRun(covMiscBg(2), n, u, reqs)
		for i, want := range []string{addrA, addrB} {
			require.NoError(t, outs[i].err)
			require.True(t, outs[i].handled, "%s: call %d is answered from the pack", block, i)
			assert.Equal(t, want, covMiscResultString(t, outs[i].resp), "%s: each caller gets only its own return data", block)
			assert.Same(t, u, outs[i].resp.Upstream())
			assert.Same(t, u, reqs[i].LastUpstream())
		}
	}
	require.Len(t, packed, 2)
	assert.Len(t, packed[0], 2, "a numbered pack carries the clients' calls only")
	require.Len(t, packed[1], 3, "a tag pack carries one more call")
	assert.Equal(t, probeTarget, packed[1][2], "the block-number call of the chain's architecture")
	assert.False(t, gotDeadline, "callers without a deadline give the pack none")
	assert.Equal(t, before+4, covMiscMcCount(u, multicall3Packed))

	h := p.tagHead(u.Id(), time.Now())
	assert.EqualValues(t, executed, h.block, "the tag pack's answer is the upstream's tag-state sample")
	assert.False(t, h.sentAt.IsZero())
	assert.Equal(t, tagStale, h.state, "%d blocks below Latest at a 1s block time is past W", latest-executed)
}

func TestCovMisc_Multicall3_PackDeadlineIsLastCallers(t *testing.T) {
	n := &queryTestNetwork{}
	now := time.Now()
	ctx1, c1 := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer c1()
	ctx2, c2 := context.WithDeadline(context.Background(), now.Add(2*time.Hour))
	defer c2()
	var deadline time.Time
	u := covMiscMcNew(t, 2, time.Minute, func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		deadline, _ = ctx.Deadline()
		return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3(covMiscTagPack(aggregate3Result{true, []byte{1}}, aggregate3Result{true, []byte{2}}))) + `"`), nil
	})
	outs := covMiscMcRun([]context.Context{ctx2, ctx1}, n, u, []*common.NormalizedRequest{
		covMiscMcCall(1, covLogsAddr("a"), "0x01", "latest"),
		covMiscMcCall(2, covLogsAddr("b"), "0x02", "latest"),
	})
	assert.True(t, outs[0].handled)
	assert.True(t, outs[1].handled)
	assert.WithinDuration(t, now.Add(2*time.Hour), deadline, time.Second)
}

func TestCovMisc_Multicall3_FailedPackSendsEveryCallAlone(t *testing.T) {
	cases := []struct {
		name    string
		forward func() (*common.NormalizedResponse, error)
	}{
		{"forward error", func() (*common.NormalizedResponse, error) { return nil, errors.New("upstream timeout") }},
		{"nil response", func() (*common.NormalizedResponse, error) { return nil, nil }},
		{"json-rpc error", func() (*common.NormalizedResponse, error) {
			return common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes(
				[]byte(`1`), nil, []byte(`{"code":-32000,"message":"execution reverted"}`))), nil
		}},
		{"result is not a string", func() (*common.NormalizedResponse, error) { return covMiscMcResult(`123`), nil }},
		{"result is not hex", func() (*common.NormalizedResponse, error) { return covMiscMcResult(`"0xzz"`), nil }},
		{"malformed aggregate3", func() (*common.NormalizedResponse, error) { return covMiscMcResult(`"0x01"`), nil }},
		{"result count mismatch", func() (*common.NormalizedResponse, error) {
			return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3([]aggregate3Result{{true, []byte{1}}})) + `"`), nil
		}},
		{"tag pack answered without its block-number entry", func() (*common.NormalizedResponse, error) {
			return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3([]aggregate3Result{{true, []byte{1}}, {true, []byte{2}}})) + `"`), nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := covMiscMcNew(t, 2, time.Minute, func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return tc.forward()
			})
			before := covMiscMcCount(u, multicall3Failed)
			outs := covMiscMcRun(covMiscBg(2), &queryTestNetwork{}, u, []*common.NormalizedRequest{
				covMiscMcCall(1, covLogsAddr("a"), "0x01", "latest"),
				covMiscMcCall(2, covLogsAddr("b"), "0x02", "latest"),
			})
			for _, o := range outs {
				assert.False(t, o.handled)
				assert.Nil(t, o.resp)
				assert.NoError(t, o.err)
			}
			assert.Equal(t, before+2, covMiscMcCount(u, multicall3Failed))
		})
	}
}

func TestCovMisc_Multicall3_NoCodeMarksOlderBlocksIneligible(t *testing.T) {
	var forwards int
	var mu sync.Mutex
	u := covMiscMcNew(t, 2, time.Minute, func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		mu.Lock()
		forwards++
		mu.Unlock()
		return covMiscMcResult(`"0x"`), nil
	})
	n := &queryTestNetwork{}
	outs := covMiscMcRun(covMiscBg(2), n, u, []*common.NormalizedRequest{
		covMiscMcCall(1, covLogsAddr("a"), "0x01", "0x10"),
		covMiscMcCall(2, covLogsAddr("b"), "0x02", "0x10"),
	})
	assert.False(t, outs[0].handled)
	assert.False(t, outs[1].handled)
	assert.Equal(t, int64(16), u.b.absentAtOrBelow.Load())

	before := covMiscMcCount(u, multicall3Ineligible)
	handled, _, err := upstreamPreForward_eth_call(context.Background(), n, u, covMiscMcCall(3, covLogsAddr("c"), "0x03", "0x5"))
	assert.False(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, before+1, covMiscMcCount(u, multicall3Ineligible), "a block before deployment is never packed")
	assert.Equal(t, 1, forwards)
}

func TestCovMisc_Multicall3_RevertedInPackLearnsBypassOnlyOnCleanAloneSuccess(t *testing.T) {
	addrA, addrB := covLogsAddr("a"), covLogsAddr("b")
	u := covMiscMcNew(t, 2, time.Minute, func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3(covMiscTagPack(aggregate3Result{false, nil}, aggregate3Result{false, nil}))) + `"`), nil
	})
	n := &queryTestNetwork{}
	reqs := []*common.NormalizedRequest{covMiscMcCall(1, addrA, "0x01", "latest"), covMiscMcCall(2, addrB, "0x02", "latest")}
	before := covMiscMcCount(u, multicall3Reverted)
	outs := covMiscMcRun(covMiscBg(2), n, u, reqs)
	assert.False(t, outs[0].handled)
	assert.False(t, outs[1].handled)
	assert.Equal(t, before+2, covMiscMcCount(u, multicall3Reverted))

	rawA, _ := hex.DecodeString(strings.TrimPrefix(addrA, "0x"))
	rawB, _ := hex.DecodeString(strings.TrimPrefix(addrB, "0x"))

	// A failed alone too (json-rpc error): the contract is not learned.
	errResp := common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes(
		[]byte(`1`), nil, []byte(`{"code":3,"message":"execution reverted"}`)))
	upstreamPostForward_eth_call(u, reqs[0], errResp, nil)
	assert.False(t, u.b.bypassed(rawA))
	// A post for A again finds nothing awaiting.
	upstreamPostForward_eth_call(u, reqs[0], covMiscMcResult(`"0x01"`), nil)
	assert.False(t, u.b.bypassed(rawA))

	// B succeeded alone: B's calls bypass the pack from now on.
	upstreamPostForward_eth_call(u, reqs[1], covMiscMcResult(`"0x01"`), nil)
	assert.True(t, u.b.bypassed(rawB))

	beforeBypass := covMiscMcCount(u, multicall3Bypassed)
	handled, _, err := upstreamPreForward_eth_call(context.Background(), n, u, covMiscMcCall(3, addrB, "0x02", "latest"))
	assert.False(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, beforeBypass+1, covMiscMcCount(u, multicall3Bypassed))
}

func TestCovMisc_Multicall3_PostForwardTransportErrorLearnsNothing(t *testing.T) {
	addrA := covLogsAddr("d")
	rawA, _ := hex.DecodeString(strings.TrimPrefix(addrA, "0x"))
	u := covMiscMcNew(t, 2, time.Minute, nil)
	u.forward = func(_ context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		// Pack order follows arrival: answer by target, A reverts.
		_, targets := covMiscAgg3Targets(t, nq)
		res := make([]aggregate3Result, len(targets))
		for i, tg := range targets {
			if string(tg) == string(rawA) {
				res[i] = aggregate3Result{success: false}
			} else {
				res[i] = aggregate3Result{success: true, data: []byte{9}}
			}
		}
		return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3(res)) + `"`), nil
	}
	reqs := []*common.NormalizedRequest{covMiscMcCall(1, addrA, "0x01", "latest"), covMiscMcCall(2, covLogsAddr("e"), "0x02", "latest")}
	outs := covMiscMcRun(covMiscBg(2), &queryTestNetwork{}, u, reqs)
	assert.False(t, outs[0].handled)
	assert.True(t, outs[1].handled)
	assert.Equal(t, "0x09", covMiscResultString(t, outs[1].resp))

	upstreamPostForward_eth_call(u, reqs[0], nil, errors.New("connection reset"))
	assert.False(t, u.b.bypassed(rawA))
}

func TestCovMisc_Multicall3_UnencodableCallerIdFallsBackAlone(t *testing.T) {
	u := covMiscMcNew(t, 2, time.Minute, func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return covMiscMcResult(`"0x` + hex.EncodeToString(covMiscAgg3(covMiscTagPack(aggregate3Result{true, []byte{1}}, aggregate3Result{true, []byte{2}}))) + `"`), nil
	})
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{map[string]interface{}{"to": covLogsAddr("a"), "data": "0x01"}, "latest"})
	jrq.ID = make(chan int)
	bad := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	before := covMiscMcCount(u, multicall3Failed)
	outs := covMiscMcRun(covMiscBg(2), &queryTestNetwork{}, u, []*common.NormalizedRequest{
		covMiscMcCall(1, covLogsAddr("b"), "0x02", "latest"), bad,
	})
	assert.True(t, outs[0].handled)
	assert.False(t, outs[1].handled, "an answer that cannot carry the caller's id is not served")
	assert.Equal(t, before+1, covMiscMcCount(u, multicall3Failed))
}

func TestCovMisc_Multicall3_AloneCanceledAndIneligible(t *testing.T) {
	n := &queryTestNetwork{}
	noForward := func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		t.Error("a lone or ineligible call must not be packed")
		return nil, nil
	}

	t.Run("a lone call is flushed by the timer and sent alone", func(t *testing.T) {
		u := covMiscMcNew(t, 5, time.Millisecond, noForward)
		before := covMiscMcCount(u, multicall3Alone)
		handled, resp, err := upstreamPreForward_eth_call(context.Background(), n, u, covMiscMcCall(1, covLogsAddr("a"), "0x01", "latest"))
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
		assert.Equal(t, before+1, covMiscMcCount(u, multicall3Alone))
	})

	t.Run("a canceled caller stops waiting", func(t *testing.T) {
		u := covMiscMcNew(t, 5, 10*time.Millisecond, noForward)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := covMiscMcCount(u, multicall3Canceled)
		handled, _, err := upstreamPreForward_eth_call(ctx, n, u, covMiscMcCall(1, covLogsAddr("a"), "0x01", "latest"))
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Equal(t, before+1, covMiscMcCount(u, multicall3Canceled))
	})

	t.Run("an unencodable block selector is ineligible", func(t *testing.T) {
		u := covMiscMcNew(t, 5, time.Minute, noForward)
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{map[string]interface{}{"to": covLogsAddr("a")}, make(chan int)})
		before := covMiscMcCount(u, multicall3Ineligible)
		handled, _, err := upstreamPreForward_eth_call(context.Background(), n, u, common.NewNormalizedRequestFromJsonRpcRequest(jrq))
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Equal(t, before+1, covMiscMcCount(u, multicall3Ineligible))
	})

	t.Run("upstreams without a batcher and pack requests pass through", func(t *testing.T) {
		rq := covMiscMcCall(1, covLogsAddr("a"), "0x01", "latest")
		handled, _, _ := upstreamPreForward_eth_call(context.Background(), n, common.NewFakeUpstream("plain"), rq)
		assert.False(t, handled)
		handled, _, _ = upstreamPreForward_eth_call(context.Background(), n, &covMiscMcUpstream{Upstream: common.NewFakeUpstream("nil-batcher")}, rq)
		assert.False(t, handled)
		u := covMiscMcNew(t, 5, time.Minute, noForward)
		pack := covMiscMcCall(2, covLogsAddr("a"), "0x01", "latest")
		pack.SetCompositeType(common.CompositeTypeMulticall3)
		handled, _, _ = upstreamPreForward_eth_call(context.Background(), n, u, pack)
		assert.False(t, handled)
		// Post-forward on such upstreams is a no-op.
		upstreamPostForward_eth_call(common.NewFakeUpstream("plain"), rq, nil, nil)
		upstreamPostForward_eth_call(&covMiscMcUpstream{Upstream: common.NewFakeUpstream("nil-batcher")}, rq, nil, nil)
	})
}

func TestCovMisc_Multicall3CallOf(t *testing.T) {
	to := covLogsAddr("a")
	cases := []struct {
		name     string
		body     string
		ok       bool
		wantData string
		wantGas  string
		wantFrom string
	}{
		{"unparseable", `{not json`, false, "", "", ""},
		{"block missing", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `"}]}`, false, "", "", ""},
		{"block null", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `"},null]}`, false, "", "", ""},
		{"call not an object", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":["` + to + `","latest"]}`, false, "", "", ""},
		{"non-string field", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","gas":21000},"latest"]}`, false, "", "", ""},
		{"value cannot be carried", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","value":"0x1"},"latest"]}`, false, "", "", ""},
		{"non-zero sender", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","from":"` + covLogsAddr("1") + `"},"latest"]}`, false, "", "", ""},
		{"data and input disagree", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","data":"0x01","input":"0x02"},"latest"]}`, false, "", "", ""},
		{"bad target", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1234"},"latest"]}`, false, "", "", ""},
		{"bad calldata", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","data":"0xzz"},"latest"]}`, false, "", "", ""},
		{"null fields ignored, zero sender and gas kept", `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"` + to + `","value":null,"gas":"0x5208","from":"0x0000000000000000000000000000000000000000","data":"0xAB","input":"0xab"},"latest"]}`, true, "ab", "0x5208", "0x0000000000000000000000000000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, data, block, gas, from, ok := multicall3CallOf(context.Background(), common.NewNormalizedRequest([]byte(tc.body)))
			require.Equal(t, tc.ok, ok)
			if !ok {
				assert.Nil(t, target)
				return
			}
			assert.Equal(t, strings.TrimPrefix(to, "0x"), hex.EncodeToString(target))
			assert.Equal(t, tc.wantData, hex.EncodeToString(data))
			assert.Equal(t, "latest", block)
			assert.Equal(t, tc.wantGas, gas)
			assert.Equal(t, tc.wantFrom, from)
		})
	}
}

func TestCovMisc_Multicall3BlockNumber(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int64
	}{
		{"0x10", 16},
		{"latest", 0},
		{map[string]interface{}{"blockNumber": "0x20"}, 32},
		{"0x" + strings.Repeat("f", 20), 0},
		{true, 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, multicall3BlockNumber(tc.in), "%v", tc.in)
	}
}

func TestCovMisc_DecodeAggregate3(t *testing.T) {
	valid := covMiscAgg3([]aggregate3Result{{true, []byte{0xaa, 0xbb}}, {false, nil}})
	got, ok := decodeAggregate3(valid)
	require.True(t, ok)
	require.Len(t, got, 2)
	assert.Equal(t, aggregate3Result{success: true, data: []byte{0xaa, 0xbb}}, got[0])
	assert.False(t, got[1].success)
	assert.Empty(t, got[1].data)

	// Layout of valid: [0]=32 array offset, [32]=count 2, [64],[96]=element
	// offsets (64, 192), element 0 at 128: success, dataRel, length, data.
	setWord := func(raw []byte, at int, v uint64) []byte {
		out := append([]byte(nil), raw...)
		copy(out[at:at+32], covMiscWord(v))
		return out
	}
	highByte := func(raw []byte, at int) []byte {
		out := append([]byte(nil), raw...)
		out[at] = 1
		return out
	}
	L := uint64(len(valid))
	cases := map[string][]byte{
		"too short":                   valid[:31],
		"array offset high bytes":     highByte(valid, 0),
		"array offset beyond input":   setWord(valid, 0, L+1),
		"count word beyond input":     setWord(valid, 0, L),
		"count exceeds head space":    setWord(valid, 32, (L-64)/32+1),
		"element offset high bytes":   highByte(valid, 64),
		"success is not a bool":       setWord(valid, 128, 2),
		"data offset high bytes":      highByte(valid, 160),
		"length word beyond input":    setWord(valid, 160, L-128),
		"data runs past end of input": setWord(valid, 192, L),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			res, ok := decodeAggregate3(raw)
			assert.False(t, ok)
			assert.Nil(t, res)
		})
	}
}

// ---------------------------------------------------------------------------
// hooks dispatch
// ---------------------------------------------------------------------------

func TestCovMisc_Hooks_UnparseableRequestFailsEveryHook(t *testing.T) {
	ctx := context.Background()
	n := &queryTestNetwork{}
	u := common.NewFakeUpstream("rpc1")
	bad := func() *common.NormalizedRequest { return common.NewNormalizedRequest([]byte(`{not json`)) }
	rs := common.NewNormalizedResponse()
	upErr := errors.New("upstream failed")

	_, _, err := HandleProjectPreForward(ctx, n, bad())
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal))
	_, _, err = HandleNetworkPreForward(ctx, n, nil, bad())
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal))
	out, err := HandleNetworkPostForward(ctx, n, bad(), rs, upErr)
	assert.Same(t, rs, out)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal))
	_, _, err = HandleUpstreamPreForward(ctx, n, u, bad(), false)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal))
	out, err = HandleUpstreamPostForward(ctx, n, u, bad(), rs, upErr, false)
	assert.Same(t, rs, out)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal))
}

func TestCovMisc_Hooks_TraceFilterDispatch(t *testing.T) {
	ctx := context.Background()
	inverted := func(method string) *common.NormalizedRequest {
		return common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(method, []interface{}{
			map[string]interface{}{"fromBlock": "0x10", "toBlock": "0x5"},
		}))
	}
	n := &queryTestNetwork{
		cfg:    &common.NetworkConfig{Evm: &common.EvmNetworkConfig{Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)}}},
		latest: 100,
		forwardFn: func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			t.Fatal("must not forward")
			return nil, nil
		},
	}
	for _, method := range []string{"trace_filter", "arbtrace_filter"} {
		t.Run(method, func(t *testing.T) {
			handled, resp, err := HandleProjectPreForward(ctx, n, common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(method, []interface{}{
				map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x5"},
			})))
			assert.False(t, handled)
			assert.Nil(t, resp)
			assert.NoError(t, err)

			handled, _, err = HandleNetworkPreForward(ctx, n, nil, inverted(method))
			assert.True(t, handled)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
			assert.Contains(t, err.Error(), "fromBlock (16) must be less than or equal to toBlock (5)")

			handled, _, err = HandleUpstreamPreForward(ctx, n, common.NewFakeUpstream("rpc1"), inverted(method), false)
			assert.True(t, handled)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))

			upErr := errors.New("upstream timeout")
			rs := common.NewNormalizedResponse()
			out, err := HandleNetworkPostForward(ctx, n, inverted(method), rs, upErr)
			assert.Same(t, rs, out)
			assert.Same(t, upErr, err, "without splitOnError the upstream error is kept")
		})
	}
}

// ---------------------------------------------------------------------------
// integrity record path (hooks.go) + export
// ---------------------------------------------------------------------------

// covMiscIntegrityNetwork carries an integrity config under a network id of
// its own (unique per call, so -count=N reruns also get a fresh slot in the
// process-wide exporter cache).
type covMiscIntegrityNetwork struct {
	common.Network
	id  string
	cfg *common.NetworkConfig
}

func (n *covMiscIntegrityNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *covMiscIntegrityNetwork) Id() string                    { return n.id }
func (n *covMiscIntegrityNetwork) ProjectId() string             { return "test-project" }
func (n *covMiscIntegrityNetwork) Label() string                 { return n.id }

var covMiscNetSeq atomic.Int64

func covMiscIndexNetwork(id, dir string, settings func(*common.IntegritySettings)) *covMiscIntegrityNetwork {
	id = fmt.Sprintf("%s-%d", id, covMiscNetSeq.Add(1))
	s := common.IntegritySettings{
		Checks: map[string]*common.IntegrityCheckConfig{"indexMagnitude": {Enabled: util.BoolPtr(true)}},
	}
	if settings != nil {
		settings(&s)
	}
	ic := &common.IntegrityConfig{IntegritySettings: s}
	if dir != "" {
		ic.MisbehaviorsDestination = &common.MisbehaviorsDestinationConfig{Type: common.MisbehaviorsDestinationTypeFile, Path: dir}
	}
	return &covMiscIntegrityNetwork{id: id, cfg: &common.NetworkConfig{Integrity: ic}}
}

func covMiscReceipt(t *testing.T, logs int) (*common.NormalizedRequest, *common.NormalizedResponse) {
	idx := make([]string, logs)
	for i := range idx {
		idx[i] = "0xfffffff7"
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(idx), nil)
	return req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func covMiscReadCatches(t *testing.T, dir string) []integrityCatchRecord {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	var out []integrityCatchRecord
	for _, f := range files {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
			if line == "" {
				continue
			}
			var rec integrityCatchRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			out = append(out, rec)
		}
	}
	return out
}

func TestCovMisc_Integrity_RecordedViolationsAreServedCountedAndArchived(t *testing.T) {
	cases := []struct {
		name     string
		settings func(*common.IntegritySettings)
		verdict  string
	}{
		{"recordOnly without autoCorrect", func(s *common.IntegritySettings) {
			s.Checks["indexMagnitude"].OnFailure = "recordOnly"
			s.AutoCorrectWhenPossible = util.BoolPtr(false)
		}, "record_only"},
		{"observeOnly suppresses a hard reject", func(s *common.IntegritySettings) {
			s.ObserveOnly = util.BoolPtr(true)
		}, "would_reject"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			n := covMiscIndexNetwork(fmt.Sprintf("evm:covmisc-rec-%d", i), dir, tc.settings)
			u := common.NewFakeUpstream("covmisc-rec-up")
			labels := []string{"test-project", "", n.id, u.Id(), "eth_gettransactionreceipt", "indexMagnitude", tc.verdict, "unknown"}
			before := testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...))

			req, rs := covMiscReceipt(t, 2)
			out, err := HandleUpstreamPostForward(context.Background(), n, u, req, rs, nil, false)
			require.NoError(t, err, "a recorded violation is served")
			assert.Same(t, rs, out)
			assert.False(t, rs.IsIntegrityRejected())
			assert.False(t, req.IntegrityCaught())
			assert.Equal(t, before+1, testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...)))

			recs := covMiscReadCatches(t, dir)
			require.Len(t, recs, 1)
			assert.Equal(t, tc.verdict, recs[0].Verdict)
			assert.Equal(t, "indexMagnitude", recs[0].Check)
			assert.Equal(t, "deterministic", recs[0].Class)
			assert.Equal(t, n.id, recs[0].Network)
			assert.Equal(t, u.Id(), recs[0].Upstream)
			assert.Contains(t, recs[0].Reason, "0xfffffff7")
			assert.Contains(t, recs[0].Response, `"logIndex":"0xfffffff7"`)
		})
	}
}

func TestCovMisc_Integrity_DetailedTracingCapsRecordedBodies(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	common.SetTracerProviderForTest(tp)
	t.Cleanup(func() { common.IsTracingEnabled = false; common.IsTracingDetailed = false })
	common.IsTracingDetailed = true

	dir := t.TempDir()
	n := covMiscIndexNetwork("evm:covmisc-trace", dir, func(s *common.IntegritySettings) {
		s.Checks["indexMagnitude"].OnFailure = "recordOnly"
		s.AutoCorrectWhenPossible = util.BoolPtr(false)
	})
	req, rs := covMiscReceipt(t, 700)
	jrr, err := rs.JsonRpcResponse()
	require.NoError(t, err)
	require.Greater(t, len(jrr.GetResultBytes()), maxTraceResponseBytes)

	_, err = HandleUpstreamPostForward(context.Background(), n, common.NewFakeUpstream("covmisc-trace-up"), req, rs, nil, false)
	require.NoError(t, err)

	var s *tracetest.SpanStub
	spans := exp.GetSpans()
	for i := range spans {
		if spans[i].Name == "Integrity.Validate" {
			s = &spans[i]
		}
	}
	require.NotNil(t, s)
	attrs := map[string]interface{}{}
	for _, a := range s.Attributes {
		attrs[string(a.Key)] = a.Value.AsInterface()
	}
	assert.Equal(t, "record_only", attrs["integrity.outcome"])
	assert.Equal(t, true, attrs["integrity.response_truncated"])
	assert.Len(t, attrs["integrity.response"], maxTraceResponseBytes)
	assert.Equal(t, "record_only", attrs["integrity.check.indexMagnitude"])
	require.Len(t, s.Events, 1)
	assert.Equal(t, "integrity.record_only", s.Events[0].Name)

	recs := covMiscReadCatches(t, dir)
	require.Len(t, recs, 1)
	assert.Len(t, recs[0].Response, maxExportBody, "the archived body is capped")
}

func TestCovMisc_ExportIntegrityCatch(t *testing.T) {
	ctx := context.Background()
	u := common.NewFakeUpstream("covmisc-exp-up")

	t.Run("no destination archives nothing", func(t *testing.T) {
		n := covMiscIndexNetwork("evm:covmisc-exp-none", "", nil)
		assert.Nil(t, networkIntegrityExporter(n))
		exportIntegrityCatch(ctx, n, u, nil, "eth_getlogs", "reject", "c", "deterministic", "unknown", "r")
	})

	t.Run("records without a response and survives a lost directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "catches")
		n := covMiscIndexNetwork("evm:covmisc-exp-file", dir, nil)
		first := networkIntegrityExporter(n)
		require.NotNil(t, first)
		assert.Same(t, first, networkIntegrityExporter(n), "one exporter per network")

		exportIntegrityCatch(ctx, n, u, nil, "eth_getLogs", "reject", "getLogsFilterSanity", "deterministic", "finalized", "log outside requested range")
		recs := covMiscReadCatches(t, dir)
		require.Len(t, recs, 1)
		assert.Equal(t, "getLogsFilterSanity", recs[0].Check)
		assert.Equal(t, "finalized", recs[0].Finality)
		assert.Empty(t, recs[0].Response)

		// Close flushes every exporter; a later catch reopens its file.
		CloseIntegrityExporters()
		require.NoError(t, os.RemoveAll(dir))
		exportIntegrityCatch(ctx, n, u, nil, "eth_getLogs", "reject", "c2", "deterministic", "unknown", "r2")
		_, statErr := os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr), "a failed append is logged, not retried into a new directory")

		require.NoError(t, os.MkdirAll(dir, 0o750))
		exportIntegrityCatch(ctx, n, u, nil, "eth_getLogs", "reject", "c3", "deterministic", "unknown", "r3")
		recs = covMiscReadCatches(t, dir)
		require.Len(t, recs, 1)
		assert.Equal(t, "c3", recs[0].Check)
		CloseIntegrityExporters()
	})
}

// ---------------------------------------------------------------------------
// pin_latest
// ---------------------------------------------------------------------------

func TestCovMisc_StateSelectorAndPinParams(t *testing.T) {
	for method, want := range map[string]int{"eth_call": 1, "eth_getStorageAt": 2, "eth_estimateGas": 1, "eth_callMany": 1, "trace_call": 2} {
		idx, ok := stateSelectorIndex(method)
		assert.True(t, ok, method)
		assert.Equal(t, want, idx, method)
	}
	_, ok := stateSelectorIndex("eth_getBlockByNumber")
	assert.False(t, ok, "block-by-number is never pinned")

	cases := []struct {
		name   string
		method string
		params []interface{}
		want   []interface{}
		pinned bool
	}{
		{"absent selector means latest", "eth_getBalance", []interface{}{"0xabc"}, []interface{}{"0xabc", "0x64"}, true},
		{"null selector means latest", "eth_getBalance", []interface{}{"0xabc", nil}, []interface{}{"0xabc", "0x64"}, true},
		{"explicit latest", "eth_getBalance", []interface{}{"0xabc", "latest"}, []interface{}{"0xabc", "0x64"}, true},
		{"eth_estimateGas latest", "eth_estimateGas", []interface{}{"0xabc", "latest"}, []interface{}{"0xabc", "0x64"}, true},
		{"params too short", "eth_getStorageAt", []interface{}{"0xabc"}, []interface{}{"0xabc"}, false},
		{"concrete block", "eth_getBalance", []interface{}{"0xabc", "0x10"}, []interface{}{"0xabc", "0x10"}, false},
		{"EIP-1898 number", "eth_getBalance", []interface{}{"0xabc", map[string]interface{}{"blockNumber": "0x1"}}, nil, false},
		{"EIP-1898 latest leaf", "eth_getBalance", []interface{}{"0xabc", map[string]interface{}{"blockNumber": "latest"}},
			[]interface{}{"0xabc", map[string]interface{}{"blockNumber": "0x64"}}, true},
		{"eth_callMany state context", "eth_callMany", []interface{}{[]interface{}{}, map[string]interface{}{"blockNumber": "latest", "transactionIndex": -1}},
			[]interface{}{[]interface{}{}, map[string]interface{}{"blockNumber": "0x64", "transactionIndex": -1}}, true},
		{"eth_callMany absent stays absent", "eth_callMany", []interface{}{[]interface{}{}}, []interface{}{[]interface{}{}}, false},
		{"not family 3", "eth_getBlockByNumber", []interface{}{"latest", false}, []interface{}{"latest", false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := deepCopyParams(tc.params)
			got, pinned := pinLatestParams(tc.method, tc.params, 100)
			assert.Equal(t, tc.pinned, pinned)
			if tc.want != nil {
				assert.Equal(t, tc.want, got)
			}
			assert.Equal(t, before, tc.params, "the input params are never mutated")
		})
	}
}

func TestCovMisc_PinBlockTagInBody(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`)
	cases := []struct {
		name   string
		body   []byte
		height int64
		pinned bool
	}{
		{"empty body", nil, 100, false},
		{"unknown height", body, 0, false},
		{"not json", []byte(`{not json`), 100, false},
		{"not family 3", []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`), 100, false},
		{"selector is not latest", []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x5"]}`), 100, false},
		{"latest is pinned", body, 100, true},
		{"absent selector is pinned", []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc"]}`), 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, pinned := PinBlockTagInBody(tc.body, tc.height)
			assert.Equal(t, tc.pinned, pinned)
			if !pinned {
				assert.Equal(t, tc.body, out)
				return
			}
			var req map[string]interface{}
			require.NoError(t, json.Unmarshal(out, &req))
			assert.Equal(t, []interface{}{"0xabc", "0x64"}, req["params"])
			assert.Equal(t, "eth_getBalance", req["method"])
		})
	}
}

func TestCovMisc_PinLatestTag(t *testing.T) {
	ctx := context.Background()
	pinUp := &queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{PinLatestTag: util.BoolPtr(true)}}}
	balance := func(block string) *common.NormalizedRequest {
		return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","` + block + `"]}`))
	}
	cases := []struct {
		name       string
		n          common.Network
		u          common.Upstream
		r          *common.NormalizedRequest
		wantParams []interface{}
		wantHeight int64
		wantPinned bool
	}{
		{"upstream does not pin", &queryTestNetwork{latest: 100}, &queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{PinLatestTag: util.BoolPtr(false)}}}, balance("latest"), nil, 0, false},
		{"unparseable request", &queryTestNetwork{latest: 100}, pinUp, common.NewNormalizedRequest([]byte(`{not json`)), nil, 0, false},
		{"concrete block is left alone", &queryTestNetwork{latest: 100}, pinUp, balance("0x5"), nil, 0, false},
		{"head unknown: must not send latest", &queryTestNetwork{}, pinUp, balance("latest"), nil, 0, true},
		{"latest pinned to head", &queryTestNetwork{latest: 100}, pinUp, balance("latest"), []interface{}{"0xabc", "0x64"}, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, height, pinned := PinLatestTag(ctx, tc.n, tc.u, tc.r)
			assert.Equal(t, tc.wantPinned, pinned)
			assert.Equal(t, tc.wantHeight, height)
			assert.Equal(t, tc.wantParams, params)
		})
	}
}

// ---------------------------------------------------------------------------
// json_rpc params helpers
// ---------------------------------------------------------------------------

type covMiscOpaque struct{ v int }

func TestCovMisc_DeepCopyParams(t *testing.T) {
	assert.Nil(t, deepCopyParams(nil))

	op := &covMiscOpaque{v: 1}
	orig := []interface{}{
		map[string]interface{}{"address": []interface{}{"0xa", nil}, "n": 1.5},
		"latest",
		op,
	}
	cp := deepCopyParams(orig)
	require.Equal(t, orig, cp)

	cp[0].(map[string]interface{})["address"].([]interface{})[0] = "0xchanged"
	cp[0].(map[string]interface{})["n"] = 2.0
	assert.Equal(t, "0xa", orig[0].(map[string]interface{})["address"].([]interface{})[0], "nested slices are copied")
	assert.Equal(t, 1.5, orig[0].(map[string]interface{})["n"], "nested maps are copied")
	assert.Same(t, op, cp[2], "unknown types are shared as-is")
}

func TestCovMisc_ReplaceParamAtPath(t *testing.T) {
	params := []interface{}{
		map[string]interface{}{"fromBlock": "latest", "nested": map[string]interface{}{"tag": "latest"}},
		"latest",
	}
	cases := []struct {
		name    string
		path    []interface{}
		changed bool
		check   func(t *testing.T, out []interface{})
	}{
		{"empty path", nil, false, nil},
		{"index out of range", []interface{}{5}, false, nil},
		{"negative index", []interface{}{-1}, false, nil},
		{"string key on non-object", []interface{}{1, "fromBlock"}, false, nil},
		{"missing nested key", []interface{}{0, "nested2", "absent"}, false, nil},
		{"missing leaf key is created", []interface{}{0, "toBlock"}, true, func(t *testing.T, out []interface{}) {
			assert.Equal(t, "0x64", out[0].(map[string]interface{})["toBlock"])
			assert.Equal(t, "latest", out[0].(map[string]interface{})["fromBlock"])
		}},
		{"index right after the last is appended", []interface{}{2}, true, func(t *testing.T, out []interface{}) {
			assert.Equal(t, []interface{}{params[0], "latest", "0x64"}, out)
		}},
		{"unsupported path element", []interface{}{0, 1.5}, false, nil},
		{"string key at root", []interface{}{"fromBlock"}, false, nil},
		{"top-level index", []interface{}{1}, true, func(t *testing.T, out []interface{}) {
			assert.Equal(t, "0x64", out[1])
		}},
		{"nested object key", []interface{}{0, "nested", "tag"}, true, func(t *testing.T, out []interface{}) {
			assert.Equal(t, "0x64", out[0].(map[string]interface{})["nested"].(map[string]interface{})["tag"])
			assert.Equal(t, "latest", out[0].(map[string]interface{})["fromBlock"])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := replaceParamAtPath(params, tc.path, "0x64")
			assert.Equal(t, tc.changed, changed)
			if !changed {
				assert.Equal(t, params, out)
				return
			}
			tc.check(t, out)
			// The input is never mutated.
			assert.Equal(t, "latest", params[1])
			assert.Equal(t, "latest", params[0].(map[string]interface{})["nested"].(map[string]interface{})["tag"])
		})
	}
}
