package erpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shadowCmpFixture is a bootstrapped project with primary rpc1 and the given
// shadow upstreams on network evm:123.
type shadowCmpFixture struct {
	ctx     context.Context
	prj     *PreparedProject
	network *Network
}

func newShadowCmpFixture(t *testing.T, projectId string, shadows ...*common.UpstreamConfig) *shadowCmpFixture {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	rlr, err := upstream.NewRateLimitersRegistry(ctx, &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	ups := []*common.UpstreamConfig{{
		Id:       "rpc1",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			StatePollerInterval: common.Duration(200 * time.Millisecond),
			StatePollerDebounce: common.Duration(50 * time.Millisecond),
		},
	}}
	for _, s := range shadows {
		s.Type = common.UpstreamTypeEvm
		if s.Evm == nil {
			s.Evm = &common.EvmUpstreamConfig{}
		}
		s.Evm.ChainId = 123
		s.Evm.StatePollerInterval = common.Duration(200 * time.Millisecond)
		s.Evm.StatePollerDebounce = common.Duration(50 * time.Millisecond)
		if s.Shadow == nil {
			s.Shadow = &common.ShadowUpstreamConfig{}
		}
		s.Shadow.Enabled = true
		ups = append(ups, s)
	}

	reg, err := NewProjectsRegistry(ctx, &log.Logger, []*common.ProjectConfig{{
		Id: projectId,
		Networks: []*common.NetworkConfig{{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
		}},
		Upstreams: ups,
	}}, ssr, nil, nil, rlr, thirdparty.NewVendorsRegistry(), nil, nil)
	require.NoError(t, err)
	reg.Bootstrap(ctx)

	prj, err := reg.GetProject(projectId)
	require.NoError(t, err)
	network, err := prj.GetNetwork(ctx, util.EvmNetworkId(123))
	require.NoError(t, err)
	require.Len(t, network.ShadowUpstreams(), len(shadows), "every shadow upstream is registered as shadow")

	return &shadowCmpFixture{ctx: ctx, prj: prj, network: network}
}

// shadowUpstream returns the registered shadow upstream with the given id.
func (f *shadowCmpFixture) shadowUpstream(t *testing.T, id string) *upstream.Upstream {
	t.Helper()
	for _, u := range f.network.ShadowUpstreams() {
		if u.Id() == id {
			return u
		}
	}
	t.Fatalf("shadow upstream %s not found", id)
	return nil
}

// shadowCmpWaitHead blocks until the upstream's state poller reports the given head.
func shadowCmpWaitHead(t *testing.T, u *upstream.Upstream, head int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		sp := u.EvmStatePoller()
		return sp != nil && sp.LatestBlock() == head
	}, 3*time.Second, 10*time.Millisecond, "poller for %s should settle on %d (poller=%v)", u.Id(), head, func() interface{} {
		if sp := u.EvmStatePoller(); sp != nil {
			return sp.LatestBlock()
		}
		return nil
	}())
}

// shadowCmpHits records the request bodies one mocked host receives for one method.
type shadowCmpHits struct {
	mu     sync.Mutex
	bodies []string
}

func (h *shadowCmpHits) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *shadowCmpHits) last() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) == 0 {
		return ""
	}
	return h.bodies[len(h.bodies)-1]
}

// shadowCmpMock answers every `method` call to `host` with `reply` (a JSON-RPC
// envelope) and records the request bodies. The mock is built whole before it
// is registered: gock.New registers first, and its Persist then writes a field
// the running state pollers' requests read.
func shadowCmpMock(host, method, reply string) *shadowCmpHits {
	h := &shadowCmpHits{}
	req, res := gock.NewRequest(), gock.NewResponse()
	mock := gock.NewMock(req, res)
	req.URL("http://" + host).Post("").Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != host {
				return false
			}
			body := util.SafeReadBody(r)
			if !strings.Contains(body, `"`+method+`"`) {
				return false
			}
			h.mu.Lock()
			h.bodies = append(h.bodies, body)
			h.mu.Unlock()
			return true
		})
	res.Status(200).JSON([]byte(reply))
	gock.Register(mock)
	return h
}

// shadowCmpCount sums the counter series of c whose labels include every pair in match.
func shadowCmpCount(c prometheus.Collector, match map[string]string) float64 {
	ch := make(chan prometheus.Metric, 256)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	total := 0.0
	for m := range ch {
		var pb dto.Metric
		if m.Write(&pb) != nil || pb.Counter == nil {
			continue
		}
		got := make(map[string]string, len(pb.GetLabel()))
		for _, lp := range pb.GetLabel() {
			got[lp.GetName()] = lp.GetValue()
		}
		ok := true
		for k, v := range match {
			if got[k] != v {
				ok = false
				break
			}
		}
		if ok {
			total += pb.Counter.GetValue()
		}
	}
	return total
}

// shadowCmpOutcome snapshots the three shadow outcome counters for one upstream+method.
type shadowCmpOutcome struct {
	identical, mismatch, errors float64
}

func shadowCmpOutcomes(upsId, method string) shadowCmpOutcome {
	sel := map[string]string{"upstream": upsId, "category": method}
	return shadowCmpOutcome{
		identical: shadowCmpCount(telemetry.MetricShadowResponseIdenticalTotal, sel),
		mismatch:  shadowCmpCount(telemetry.MetricShadowResponseMismatchTotal, sel),
		errors:    shadowCmpCount(telemetry.MetricShadowResponseErrorTotal, sel),
	}
}

// shadowCmpAwait waits until the outcome counters of upsId/method moved by want
// relative to before, then asserts they moved by exactly want.
func shadowCmpAwait(t *testing.T, upsId, method string, before, want shadowCmpOutcome) {
	t.Helper()
	diff := func() shadowCmpOutcome {
		now := shadowCmpOutcomes(upsId, method)
		return shadowCmpOutcome{
			identical: now.identical - before.identical,
			mismatch:  now.mismatch - before.mismatch,
			errors:    now.errors - before.errors,
		}
	}
	require.Eventually(t, func() bool {
		d := diff()
		return d.identical+d.mismatch+d.errors >= want.identical+want.mismatch+want.errors
	}, 3*time.Second, 10*time.Millisecond, "shadow outcome for %s/%s never recorded", upsId, method)
	assert.Equal(t, want, diff(), "shadow outcome for %s/%s", upsId, method)
}

// shadowCmpResponse builds a primary response for body with the given raw
// JSON-RPC result or error.
func shadowCmpResponse(network *Network, body, result, rpcErr string) *common.NormalizedResponse {
	req := common.NewNormalizedRequest([]byte(body))
	req.SetNetwork(network)
	var res, errB []byte
	if result != "" {
		res = []byte(result)
	}
	if rpcErr != "" {
		errB = []byte(rpcErr)
	}
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), res, errB)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

// shadowCmpResultOf returns the raw "result" member of a JSON-RPC envelope.
func shadowCmpResultOf(t *testing.T, envelope string) string {
	t.Helper()
	var env map[string]interface{}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(envelope), &env))
	out, err := common.SonicCfg.Marshal(env["result"])
	require.NoError(t, err)
	return string(out)
}

func shadowCmpForward(t *testing.T, f *shadowCmpFixture, method, params string) string {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}`, method, params)))
	resp, err := f.prj.Forward(f.ctx, util.EvmNetworkId(123), req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse(f.ctx)
	require.NoError(t, err)
	return jrr.GetResultString()
}

// TestShadowCompare_ForwardPath drives PreparedProject.Forward with a primary
// and two shadows (one ignoring the "ts" field of shadowcmp_obj) and checks
// the client answer comes from the primary while each shadow's comparison
// outcome lands in the matching counter.
func TestShadowCompare_ForwardPath(t *testing.T) {
	f := newShadowCmpFixture(t, "shadowcmp_fwd",
		&common.UpstreamConfig{
			Id:       "shadowcmp-fwd-ign",
			Endpoint: "http://rpc2.localhost",
			Shadow: &common.ShadowUpstreamConfig{
				IgnoreFields: map[string][]string{"shadowcmp_obj": {"ts"}},
			},
		},
		&common.UpstreamConfig{
			Id:       "shadowcmp-fwd-plain",
			Endpoint: "http://rpc3.localhost",
		},
	)
	const ign, plain = "shadowcmp-fwd-ign", "shadowcmp-fwd-plain"

	cases := []struct {
		name          string
		method        string
		params        string
		primary       string
		shadow        string
		wantIgn       shadowCmpOutcome
		wantPlain     shadowCmpOutcome
		mismatchLabel map[string]string
	}{
		{
			name:      "IdenticalResult",
			method:    "shadowcmp_same",
			params:    `[]`,
			primary:   `{"jsonrpc":"2.0","id":1,"result":"0x10"}`,
			shadow:    `{"jsonrpc":"2.0","id":1,"result":"0x10"}`,
			wantIgn:   shadowCmpOutcome{identical: 1},
			wantPlain: shadowCmpOutcome{identical: 1},
		},
		{
			name:          "LargerShadowResultMismatches",
			method:        "shadowcmp_larger",
			params:        `[]`,
			primary:       `{"jsonrpc":"2.0","id":1,"result":"0x10"}`,
			shadow:        `{"jsonrpc":"2.0","id":1,"result":"0x1000"}`,
			wantIgn:       shadowCmpOutcome{mismatch: 1},
			wantPlain:     shadowCmpOutcome{mismatch: 1},
			mismatchLabel: map[string]string{"emptyish": "false", "larger": "true"},
		},
		{
			name:          "EmptyShadowAgainstNonEmptyPrimaryMismatches",
			method:        "shadowcmp_emptyshadow",
			params:        `[]`,
			primary:       `{"jsonrpc":"2.0","id":1,"result":"0x10"}`,
			shadow:        `{"jsonrpc":"2.0","id":1,"result":"0x0"}`,
			wantIgn:       shadowCmpOutcome{mismatch: 1},
			wantPlain:     shadowCmpOutcome{mismatch: 1},
			mismatchLabel: map[string]string{"emptyish": "true", "larger": "false"},
		},
		{
			name:      "BothEmptyishWithDifferentBytesAreIdentical",
			method:    "shadowcmp_bothempty",
			params:    `[]`,
			primary:   `{"jsonrpc":"2.0","id":1,"result":"0x0"}`,
			shadow:    `{"jsonrpc":"2.0","id":1,"result":"0x"}`,
			wantIgn:   shadowCmpOutcome{identical: 1},
			wantPlain: shadowCmpOutcome{identical: 1},
		},
		{
			name:      "IgnoredFieldDifferenceOnlyMatchesWhenConfigured",
			method:    "shadowcmp_obj",
			params:    `[]`,
			primary:   `{"jsonrpc":"2.0","id":1,"result":{"v":"0x1","ts":"0x5"}}`,
			shadow:    `{"jsonrpc":"2.0","id":1,"result":{"v":"0x1","ts":"0x6"}}`,
			wantIgn:   shadowCmpOutcome{identical: 1},
			wantPlain: shadowCmpOutcome{mismatch: 1},
		},
		{
			name:      "ShadowServerErrorCountsAsError",
			method:    "shadowcmp_err",
			params:    `[]`,
			primary:   `{"jsonrpc":"2.0","id":1,"result":"0x10"}`,
			shadow:    `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal failure"}}`,
			wantIgn:   shadowCmpOutcome{errors: 1},
			wantPlain: shadowCmpOutcome{errors: 1},
		},
		{
			name:          "ShadowRevertAgainstPrimaryResultMismatches",
			method:        "eth_call",
			params:        `[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]`,
			primary:       `{"jsonrpc":"2.0","id":1,"result":"0x01"}`,
			shadow:        `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted","data":"0x"}}`,
			wantIgn:       shadowCmpOutcome{mismatch: 1},
			wantPlain:     shadowCmpOutcome{mismatch: 1},
			mismatchLabel: map[string]string{"finality": "n/a", "emptyish": "false", "larger": "false"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shadowCmpMock("rpc1.localhost", tc.method, tc.primary)
			shadowCmpMock("rpc2.localhost", tc.method, tc.shadow)
			shadowCmpMock("rpc3.localhost", tc.method, tc.shadow)

			beforeIgn := shadowCmpOutcomes(ign, tc.method)
			beforePlain := shadowCmpOutcomes(plain, tc.method)
			var beforeLabeled float64
			if tc.mismatchLabel != nil {
				sel := map[string]string{"upstream": plain, "category": tc.method}
				for k, v := range tc.mismatchLabel {
					sel[k] = v
				}
				beforeLabeled = shadowCmpCount(telemetry.MetricShadowResponseMismatchTotal, sel)
			}

			got := shadowCmpForward(t, f, tc.method, tc.params)
			assert.JSONEq(t, shadowCmpResultOf(t, tc.primary), got, "client receives the primary's result")

			shadowCmpAwait(t, ign, tc.method, beforeIgn, tc.wantIgn)
			shadowCmpAwait(t, plain, tc.method, beforePlain, tc.wantPlain)

			if tc.mismatchLabel != nil {
				sel := map[string]string{"upstream": plain, "category": tc.method}
				for k, v := range tc.mismatchLabel {
					sel[k] = v
				}
				assert.Equal(t, 1.0, shadowCmpCount(telemetry.MetricShadowResponseMismatchTotal, sel)-beforeLabeled,
					"mismatch carries labels %v", tc.mismatchLabel)
			}
		})
	}
}

// TestShadowCompare_SkipConditions calls executeShadowRequests directly with a
// concrete-height eth_getBalance. Only the control shadow may be sent the
// request; the ignored-method, zero-sample-rate, out-of-range and
// invalid-pattern shadows are skipped before any goroutine is spawned.
func TestShadowCompare_SkipConditions(t *testing.T) {
	zero := 0.0
	f := newShadowCmpFixture(t, "shadowcmp_skip",
		&common.UpstreamConfig{
			Id:            "shadowcmp-skip-ignored",
			Endpoint:      "http://rpc2.localhost",
			IgnoreMethods: []string{"eth_getBalance"},
		},
		&common.UpstreamConfig{
			Id:       "shadowcmp-skip-sampled",
			Endpoint: "http://rpc3.localhost",
			Shadow:   &common.ShadowUpstreamConfig{SampleRate: &zero},
		},
		&common.UpstreamConfig{
			Id:       "shadowcmp-skip-recent",
			Endpoint: "http://rpc4.localhost",
			Evm:      &common.EvmUpstreamConfig{MaxAvailableRecentBlocks: 100},
		},
		&common.UpstreamConfig{
			Id:            "shadowcmp-skip-badpattern",
			Endpoint:      "http://rpc5.localhost",
			IgnoreMethods: []string{"(eth_getBalance"},
		},
		&common.UpstreamConfig{
			Id:       "shadowcmp-skip-control",
			Endpoint: "http://rpc7.localhost",
		},
	)
	control := f.shadowUpstream(t, "shadowcmp-skip-control")
	shadowCmpWaitHead(t, control, 0x33338888)
	shadowCmpWaitHead(t, f.shadowUpstream(t, "shadowcmp-skip-recent"), 0x33338888)

	const method = "eth_getBalance"
	reply := `{"jsonrpc":"2.0","id":1,"result":"0x64"}`
	skipped := map[string]*shadowCmpHits{
		"shadowcmp-skip-ignored":    shadowCmpMock("rpc2.localhost", method, reply),
		"shadowcmp-skip-sampled":    shadowCmpMock("rpc3.localhost", method, reply),
		"shadowcmp-skip-recent":     shadowCmpMock("rpc4.localhost", method, reply),
		"shadowcmp-skip-badpattern": shadowCmpMock("rpc5.localhost", method, reply),
	}
	controlHits := shadowCmpMock("rpc7.localhost", method, reply)

	before := map[string]shadowCmpOutcome{}
	for id := range skipped {
		before[id] = shadowCmpOutcomes(id, method)
	}
	beforeControl := shadowCmpOutcomes("shadowcmp-skip-control", method)

	resp := shadowCmpResponse(f.network,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","0x1"]}`,
		`"0x64"`, "")
	f.prj.executeShadowRequests(f.ctx, f.network, f.network.ShadowUpstreams(), resp)

	shadowCmpAwait(t, "shadowcmp-skip-control", method, beforeControl, shadowCmpOutcome{identical: 1})
	assert.Equal(t, 1, controlHits.count(), "control shadow is sent the request once")
	for id, hits := range skipped {
		assert.Equal(t, 0, hits.count(), "%s must not be sent the request", id)
		assert.Equal(t, before[id], shadowCmpOutcomes(id, method), "%s records no outcome", id)
	}
}

// shadowCmpWriter is a result writer whose Size fails or panics.
type shadowCmpWriter struct {
	sizeErr  error
	panicMsg string
}

func (w *shadowCmpWriter) WriteTo(io.Writer, bool) (int64, error) { return 0, nil }
func (w *shadowCmpWriter) IsResultEmptyish() bool                   { return false }
func (w *shadowCmpWriter) Size(...context.Context) (int, error) {
	if w.panicMsg != "" {
		panic(w.panicMsg)
	}
	return 0, w.sizeErr
}

// TestShadowCompare_EarlyReturns covers the inputs executeShadowRequests
// refuses before mirroring: no response, no shadows, no request, an
// unsizable primary, and a panic (recovered and counted).
func TestShadowCompare_EarlyReturns(t *testing.T) {
	f := newShadowCmpFixture(t, "shadowcmp_early", &common.UpstreamConfig{
		Id:       "shadowcmp-early",
		Endpoint: "http://rpc2.localhost",
	})
	const method = "shadowcmp_early"
	hits := shadowCmpMock("rpc2.localhost", method, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	shadows := f.network.ShadowUpstreams()
	body := `{"jsonrpc":"2.0","id":1,"method":"shadowcmp_early","params":[]}`

	writerResp := func(w *shadowCmpWriter) *common.NormalizedResponse {
		req := common.NewNormalizedRequest([]byte(body))
		req.SetNetwork(f.network)
		jrr := &common.JsonRpcResponse{}
		jrr.SetResultWriter(w)
		return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	}
	panicSel := map[string]string{"scope": "shadow-upstreams", "extra": "network:" + f.network.Label()}

	cases := []struct {
		name      string
		shadows   []*upstream.Upstream
		resp      *common.NormalizedResponse
		wantPanic float64
	}{
		{name: "NilResponse", shadows: shadows, resp: nil},
		{name: "NoShadowUpstreams", shadows: nil, resp: shadowCmpResponse(f.network, body, `"0x1"`, "")},
		{
			name:    "ResponseWithoutRequest",
			shadows: shadows,
			resp: common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`"0x1"`), nil)),
		},
		{name: "PrimarySizeError", shadows: shadows, resp: writerResp(&shadowCmpWriter{sizeErr: errors.New("size unavailable")})},
		{name: "PanicIsRecoveredAndCounted", shadows: shadows, resp: writerResp(&shadowCmpWriter{panicMsg: "shadowcmp boom"}), wantPanic: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := shadowCmpOutcomes("shadowcmp-early", method)
			beforePanic := shadowCmpCount(telemetry.MetricUnexpectedPanicTotal, panicSel)

			require.NotPanics(t, func() {
				f.prj.executeShadowRequests(f.ctx, f.network, tc.shadows, tc.resp)
			})

			assert.Equal(t, 0, hits.count(), "no shadow request is sent")
			assert.Equal(t, before, shadowCmpOutcomes("shadowcmp-early", method), "no shadow outcome is recorded")
			assert.Equal(t, tc.wantPanic, shadowCmpCount(telemetry.MetricUnexpectedPanicTotal, panicSel)-beforePanic)
		})
	}
}

// TestShadowCompare_DirectVerdicts covers comparison outcomes only reachable
// with a hand-built primary response: both sides reverting, a primary whose
// hash cannot be computed, a body-less primary request, and PinBlockTag.
func TestShadowCompare_DirectVerdicts(t *testing.T) {
	f := newShadowCmpFixture(t, "shadowcmp_direct", &common.UpstreamConfig{
		Id:       "shadowcmp-direct",
		Endpoint: "http://rpc2.localhost",
		Shadow: &common.ShadowUpstreamConfig{
			PinBlockTag:  true,
			IgnoreFields: map[string][]string{"shadowcmp_ignhash": {"ts"}},
		},
	})
	const upsId = "shadowcmp-direct"
	shadow := f.shadowUpstream(t, upsId)
	shadowCmpWaitHead(t, shadow, 0x22228888)
	shadows := f.network.ShadowUpstreams()

	t.Run("BothRevertedIsIdentical", func(t *testing.T) {
		const method = "eth_estimateGas"
		shadowCmpMock("rpc2.localhost", method,
			`{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted","data":"0x"}}`)
		before := shadowCmpOutcomes(upsId, method)
		resp := shadowCmpResponse(f.network,
			`{"jsonrpc":"2.0","id":1,"method":"eth_estimateGas","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"}]}`,
			"", `{"code":3,"message":"execution reverted"}`)
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, resp)
		shadowCmpAwait(t, upsId, method, before, shadowCmpOutcome{identical: 1})
	})

	// A primary without a result cannot be hashed; the comparison proceeds
	// against an empty expected hash and reports a mismatch with the
	// non-empty shadow, with and without ignore fields.
	for _, method := range []string{"shadowcmp_nohash", "shadowcmp_ignhash"} {
		t.Run("PrimaryHashFailureMismatches/"+method, func(t *testing.T) {
			shadowCmpMock("rpc2.localhost", method, `{"jsonrpc":"2.0","id":1,"result":{"v":"0x1","ts":"0x2"}}`)
			before := shadowCmpOutcomes(upsId, method)
			resp := shadowCmpResponse(f.network,
				fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":[]}`, method), "", "")
			f.prj.executeShadowRequests(f.ctx, f.network, shadows, resp)
			shadowCmpAwait(t, upsId, method, before, shadowCmpOutcome{mismatch: 1})
		})
	}

	t.Run("BodylessRequestIsMarshalledWithDirectives", func(t *testing.T) {
		const method = "shadowcmp_bodyless"
		hits := shadowCmpMock("rpc2.localhost", method, `{"jsonrpc":"2.0","id":1,"result":"0x7"}`)
		before := shadowCmpOutcomes(upsId, method)
		req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(method, []interface{}{"0xabc"}))
		req.SetNetwork(f.network)
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(
			common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`"0x7"`), nil))
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, resp)
		shadowCmpAwait(t, upsId, method, before, shadowCmpOutcome{identical: 1})
		assert.Contains(t, hits.last(), `"0xabc"`, "shadow receives the marshalled params")
	})

	// The mirror copies the original body only while it is still unparsed;
	// a request whose block ref/number are already cached is never parsed
	// by the availability check, so its body survives to be pinned.
	pinBody := `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`
	pinResp := func(blockNumber int64) *common.NormalizedResponse {
		resp := shadowCmpResponse(f.network, pinBody, `"0x5"`, "")
		resp.Request().SetEvmBlockRef("latest")
		resp.Request().SetEvmBlockNumber(int64(0x1000))
		resp.Request().SetDirectives(&common.RequestDirectives{RetryEmpty: true})
		if blockNumber > 0 {
			resp.SetEvmBlockNumber(blockNumber)
		}
		return resp
	}

	// One persistent mock serves both pin subtests: gock answers with the
	// first matching mock, so a second registration would never be hit.
	pinHits := shadowCmpMock("rpc2.localhost", "eth_getBalance", `{"jsonrpc":"2.0","id":1,"result":"0x5"}`)

	t.Run("PinBlockTagUsesPrimaryBlockNumber", func(t *testing.T) {
		before := shadowCmpOutcomes(upsId, "eth_getBalance")
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, pinResp(0x1234))
		shadowCmpAwait(t, upsId, "eth_getBalance", before, shadowCmpOutcome{identical: 1})
		assert.Contains(t, pinHits.last(), `"0x1234"`, "latest is pinned to the primary's block number")
		assert.NotContains(t, pinHits.last(), `"latest"`)
	})

	// An answer with no number of its own: the primary was sent the
	// request's Latest, not the serving upstream's head.
	var primary common.Upstream
	for _, u := range f.prj.upstreamsRegistry.GetNetworkUpstreams(f.ctx, util.EvmNetworkId(123)) {
		if u.Id() == "rpc1" {
			shadowCmpWaitHead(t, u, 0x11118888)
			primary = u
		}
	}
	require.NotNil(t, primary)
	headResp := func(dirs *common.RequestDirectives) *common.NormalizedResponse {
		resp := pinResp(0)
		resp.Request().SetDirectives(dirs)
		resp.Request().TakeEvmHead(&common.EvmHead{Latest: 0x2000})
		resp.SetUpstream(primary)
		return resp
	}

	t.Run("PinBlockTagFallsBackToRequestLatest", func(t *testing.T) {
		before := shadowCmpOutcomes(upsId, "eth_getBalance")
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, headResp(&common.RequestDirectives{RetryEmpty: true}))
		shadowCmpAwait(t, upsId, "eth_getBalance", before, shadowCmpOutcome{identical: 1})
		assert.Contains(t, pinHits.last(), `"0x2000"`, "latest is pinned to the request's Latest, the number the primary was sent")
	})

	t.Run("PinBlockTagUsesServingUpstreamHeadWhenTagReachedIt", func(t *testing.T) {
		before := shadowCmpOutcomes(upsId, "eth_getBalance")
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, headResp(&common.RequestDirectives{RetryEmpty: true, SkipInterpolation: true}))
		shadowCmpAwait(t, upsId, "eth_getBalance", before, shadowCmpOutcome{identical: 1})
		assert.Contains(t, pinHits.last(), `"0x11118888"`, "skip-interpolation sent the tag as is: latest is pinned to the serving upstream's head")
	})

	t.Run("UnresolvableRequestIsNotMirrored", func(t *testing.T) {
		const method = "shadowcmp_unresolvable"
		hits := shadowCmpMock("rpc2.localhost", method, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
		before := shadowCmpOutcomes(upsId, "")
		req := common.NewNormalizedRequest(nil)
		req.SetNetwork(f.network)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(
			common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`"0x1"`), nil))
		f.prj.executeShadowRequests(f.ctx, f.network, shadows, resp)
		// The clone fails inside the mirror goroutine; give it time to run.
		require.Never(t, func() bool { return hits.count() > 0 }, 300*time.Millisecond, 20*time.Millisecond)
		assert.Equal(t, before, shadowCmpOutcomes(upsId, ""), "no outcome is recorded")
	})
}

// TestShadowCompare_ForwardPathPinsLatestTag sends an eth_getBalance at
// "latest" through PreparedProject.Forward to a primary plus a PinBlockTag
// shadow: the client gets the primary's answer and the shadow is sent a
// concrete height in place of the tag.
func TestShadowCompare_ForwardPathPinsLatestTag(t *testing.T) {
	f := newShadowCmpFixture(t, "shadowcmp_pinfwd", &common.UpstreamConfig{
		Id:       "shadowcmp-pinfwd",
		Endpoint: "http://rpc2.localhost",
		Shadow:   &common.ShadowUpstreamConfig{PinBlockTag: true},
	})
	for _, u := range f.prj.upstreamsRegistry.GetNetworkUpstreams(f.ctx, util.EvmNetworkId(123)) {
		shadowCmpWaitHead(t, u, 0x11118888)
	}
	shadowCmpMock("rpc1.localhost", "eth_getBalance", `{"jsonrpc":"2.0","id":1,"result":"0x5"}`)
	hits := shadowCmpMock("rpc2.localhost", "eth_getBalance", `{"jsonrpc":"2.0","id":1,"result":"0x5"}`)
	before := shadowCmpOutcomes("shadowcmp-pinfwd", "eth_getBalance")

	got := shadowCmpForward(t, f, "eth_getBalance", `["0x0000000000000000000000000000000000000001","latest"]`)
	assert.Equal(t, `"0x5"`, got)

	shadowCmpAwait(t, "shadowcmp-pinfwd", "eth_getBalance", before, shadowCmpOutcome{identical: 1})
	var sent struct {
		Params []interface{} `json:"params"`
	}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(hits.last()), &sent))
	require.Len(t, sent.Params, 2)
	tag, _ := sent.Params[1].(string)
	assert.NotEqual(t, "latest", tag, "shadow copy is pinned")
	assert.True(t, strings.HasPrefix(tag, "0x"), "shadow copy carries a hex height, got %q", tag)
}
