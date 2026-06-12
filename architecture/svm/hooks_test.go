package svm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

func TestHandleProjectPreForward_GetGenesisHashShortCircuitsForKnownCluster(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	h := &SvmArchitectureHandler{}
	handled, resp, err := h.HandleProjectPreForward(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatalf("expected getGenesisHash to be short-circuited for mainnet-beta")
	}
	if resp == nil {
		t.Fatal("expected synthetic response, got nil")
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("read jsonrpc response: %v", err)
	}
	got := strings.Trim(string(jrr.GetResultBytes()), `"`)
	want, _ := common.KnownGenesisHash("", "mainnet-beta")
	if got != want {
		t.Fatalf("short-circuited hash mismatch: got %q want %q", got, want)
	}
}

func TestHandleProjectPreForward_UnknownClusterFallsThrough(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Cluster: "my-localnet"},
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	h := &SvmArchitectureHandler{}
	handled, _, err := h.HandleProjectPreForward(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatal("expected unknown cluster to fall through to upstream, not be handled")
	}
}

func TestHandleUpstreamPostForward_SendTransactionError_IsMarkedNonRetryable(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["base64tx"]}`))
	h := &SvmArchitectureHandler{}

	upstreamErr := common.NewErrEndpointServerSideException(nil, nil, 500)
	_, err := h.HandleUpstreamPostForward(context.Background(), net, nil, req, nil, upstreamErr, false)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatalf("sendTransaction error must be non-retryable toward network, got %T: %v", err, err)
	}
}

func TestNetworkPreForward_InjectCommitment_AppendsWhenNoOptionsPresent(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))

	handled, _, err := networkPreForward_injectCommitment(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatal("inject must never short-circuit the request")
	}

	jrq, err := req.JsonRpcRequest(context.Background())
	if err != nil {
		t.Fatalf("JsonRpcRequest: %v", err)
	}
	if len(jrq.Params) != 2 {
		t.Fatalf("expected 2 params after injection, got %d: %+v", len(jrq.Params), jrq.Params)
	}
	last, ok := jrq.Params[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected map param appended, got %T", jrq.Params[1])
	}
	if last["commitment"] != "finalized" {
		t.Fatalf("expected commitment:finalized, got %v", last["commitment"])
	}
}

func TestNetworkPreForward_InjectCommitment_MutatesExistingOptionsMap(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"encoding":"base64"}]}`))

	_, _, err := networkPreForward_injectCommitment(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jrq, _ := req.JsonRpcRequest(context.Background())
	opts := jrq.Params[1].(map[string]interface{})
	if opts["encoding"] != "base64" {
		t.Fatal("injection clobbered existing option keys")
	}
	if opts["commitment"] != "confirmed" {
		t.Fatalf("expected commitment:confirmed in existing map, got %v", opts["commitment"])
	}
}

func TestNetworkPreForward_InjectCommitment_RespectsCallerChoice(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	// User explicitly asked for "processed" — network default must not override.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"commitment":"processed"}]}`))

	_, _, err := networkPreForward_injectCommitment(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jrq, _ := req.JsonRpcRequest(context.Background())
	opts := jrq.Params[1].(map[string]interface{})
	if opts["commitment"] != "processed" {
		t.Fatalf("caller's commitment must win over network default, got %v", opts["commitment"])
	}
}

func TestNetworkPreForward_InjectCommitment_SkipsWriteMethods(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	writeMethods := []string{"sendTransaction", "sendRawTransaction", "simulateTransaction", "requestAirdrop"}
	for _, m := range writeMethods {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":["base64tx"]}`, m)
		req := common.NewNormalizedRequest([]byte(body))

		_, _, err := networkPreForward_injectCommitment(context.Background(), net, req)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", m, err)
		}

		jrq, _ := req.JsonRpcRequest(context.Background())
		if len(jrq.Params) != 1 {
			t.Fatalf("%s: params must not be rewritten, got %d params: %+v", m, len(jrq.Params), jrq.Params)
		}
	}
}

// TestNetworkPreForward_InjectCommitment_ShapeAware is the regression guard for
// the method-aware param-shaping fix. The injector must respect each method's
// documented options-object position and never produce an invalid RPC shape.
func TestNetworkPreForward_InjectCommitment_ShapeAware(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	paramsOf := func(method, params string) []interface{} {
		req := common.NewNormalizedRequest([]byte(
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
		if _, _, err := networkPreForward_injectCommitment(context.Background(), net, req); err != nil {
			t.Fatalf("%s: unexpected error: %v", method, err)
		}
		jrq, _ := req.JsonRpcRequest(context.Background())
		return jrq.Params
	}

	// getInflationRate takes NO params — must be left completely untouched
	// (it is no longer in the injectable table).
	if p := paramsOf("getInflationRate", "[]"); len(p) != 0 {
		t.Errorf("getInflationRate: params must stay empty, got %+v", p)
	}

	// Legacy getBlock(slot, "encoding") / getTransaction(sig, "encoding"): the
	// options slot is occupied by a string, so injection must be skipped rather
	// than appending an invalid 3rd param.
	if p := paramsOf("getBlock", `[123, "base64"]`); len(p) != 2 {
		t.Errorf("getBlock legacy form: expected 2 params untouched, got %+v", p)
	}
	if p := paramsOf("getTransaction", `["sig", "json"]`); len(p) != 2 {
		t.Errorf("getTransaction legacy form: expected 2 params untouched, got %+v", p)
	}

	// getBlock with a proper config object: commitment merged into it.
	if p := paramsOf("getBlock", `[123, {"encoding":"json"}]`); len(p) != 2 {
		t.Fatalf("getBlock object form: expected 2 params, got %+v", p)
	} else if m, ok := p[1].(map[string]interface{}); !ok || m["commitment"] != "confirmed" || m["encoding"] != "json" {
		t.Errorf("getBlock object form: expected commitment merged, got %+v", p[1])
	}

	// getTokenAccountsByOwner: options object is the 3rd param (index 2); the
	// 2nd param is the required filter and must NOT receive commitment.
	p := paramsOf("getTokenAccountsByOwner", `["owner", {"mint":"x"}]`)
	if len(p) != 3 {
		t.Fatalf("getTokenAccountsByOwner: expected options appended at index 2, got %+v", p)
	}
	if filter, ok := p[1].(map[string]interface{}); !ok || filter["commitment"] != nil {
		t.Errorf("getTokenAccountsByOwner: filter object must not get commitment, got %+v", p[1])
	}
	if opts, ok := p[2].(map[string]interface{}); !ok || opts["commitment"] != "confirmed" {
		t.Errorf("getTokenAccountsByOwner: commitment must go in options at index 2, got %+v", p[2])
	}

	// getSlot has no positional args — options object appended at index 0.
	if p := paramsOf("getSlot", "[]"); len(p) != 1 {
		t.Errorf("getSlot: expected options appended, got %+v", p)
	} else if m, ok := p[0].(map[string]interface{}); !ok || m["commitment"] != "confirmed" {
		t.Errorf("getSlot: expected {commitment} at index 0, got %+v", p[0])
	}

	// getBlocks variable arity: [start, end] (both numbers) → options appended.
	if p := paramsOf("getBlocks", `[100, 110]`); len(p) != 3 {
		t.Errorf("getBlocks [start,end]: expected options appended at index 2, got %+v", p)
	}
}

// TestNetworkPreForward_InjectCommitment_InvalidatesCacheHash guards against
// a subtle cache-key poisoning bug: CacheHash memoizes its result via
// atomic.Value on first call. If the commitment hook mutates Params without
// invalidating the memoized hash, subsequent cache lookups would key on the
// pre-mutation params — meaning two logically identical requests (one with
// the caller's explicit commitment, one where we injected it) would hit
// different cache entries.
func TestNetworkPreForward_InjectCommitment_InvalidatesCacheHash(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))

	// Prime the hash against the pre-mutation params.
	jrqBefore, err := req.JsonRpcRequest(context.Background())
	if err != nil {
		t.Fatalf("JsonRpcRequest: %v", err)
	}
	hashBefore, err := jrqBefore.CacheHash(context.Background())
	if err != nil {
		t.Fatalf("CacheHash (pre): %v", err)
	}

	if _, _, err := networkPreForward_injectCommitment(context.Background(), net, req); err != nil {
		t.Fatalf("injectCommitment: %v", err)
	}

	jrqAfter, _ := req.JsonRpcRequest(context.Background())
	hashAfter, err := jrqAfter.CacheHash(context.Background())
	if err != nil {
		t.Fatalf("CacheHash (post): %v", err)
	}

	if hashBefore == hashAfter {
		t.Fatalf("CacheHash must change after param mutation; got %q both times", hashBefore)
	}
}

func TestNetworkPreForward_InjectCommitment_NoopWithoutNetworkDefault(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{}, // no commitment set
	}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))

	_, _, err := networkPreForward_injectCommitment(context.Background(), net, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	jrq, _ := req.JsonRpcRequest(context.Background())
	if len(jrq.Params) != 1 {
		t.Fatalf("no network default → no injection; got %d params", len(jrq.Params))
	}
}

func TestNetworkPreForward_ValidateSignaturesForAddress_RejectsOversizedWindow(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm: &common.SvmNetworkConfig{
				MaxSlotsPerSignaturesQuery: 1000,
			},
		},
		latestSlot: 10_000,
	}
	// minContextSlot is 8000 slots behind latest → window = 2000, exceeds cap.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pubkey",{"minContextSlot":2000}]}`))

	handled, _, err := networkPreForward_validateSignaturesForAddress(context.Background(), net, req)
	if !handled {
		t.Fatal("expected oversized slot window to be rejected (handled=true)")
	}
	if err == nil {
		t.Fatal("expected rejection error, got nil")
	}
	if !strings.Contains(err.Error(), "maxSlotsPerSignaturesQuery") {
		t.Fatalf("error should mention the cap name, got: %v", err)
	}
}

func TestNetworkPreForward_ValidateSignaturesForAddress_AllowsWindowAtLimit(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{MaxSlotsPerSignaturesQuery: 1000},
		},
		latestSlot: 10_000,
	}
	// Window = exactly 1000 (at the cap). Must pass.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pubkey",{"minContextSlot":9000}]}`))

	handled, _, err := networkPreForward_validateSignaturesForAddress(context.Background(), net, req)
	if err != nil {
		t.Fatalf("at-cap window must not error, got: %v", err)
	}
	if handled {
		t.Fatal("at-cap window must not short-circuit the request")
	}
}

func TestNetworkPreForward_ValidateSignaturesForAddress_NoOpWithoutConfig(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg:        &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{}},
		latestSlot: 10_000,
	}
	// No cap configured → validator must not reject even obviously huge windows.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pubkey",{"minContextSlot":1}]}`))

	handled, _, err := networkPreForward_validateSignaturesForAddress(context.Background(), net, req)
	if handled || err != nil {
		t.Fatalf("no cap → no rejection; got handled=%v err=%v", handled, err)
	}
}

func TestNetworkPreForward_ValidateSignaturesForAddress_SkipsWithoutMinContextSlot(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{MaxSlotsPerSignaturesQuery: 100},
		},
		latestSlot: 10_000,
	}
	// Caller didn't specify minContextSlot → no slot-window bound to validate.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pubkey",{"limit":10}]}`))

	handled, _, err := networkPreForward_validateSignaturesForAddress(context.Background(), net, req)
	if handled || err != nil {
		t.Fatalf("no minContextSlot → no check; got handled=%v err=%v", handled, err)
	}
}

// svmUpstreamStub satisfies common.SvmUpstream so upstreamPostForward_trackContextSlot
// can reach SvmStatePoller. Only the methods the hook actually touches are
// meaningfully wired.
type svmUpstreamStub struct {
	poller common.SvmStatePoller
}

func (s *svmUpstreamStub) Id() string                                       { return "svm-stub" }
func (s *svmUpstreamStub) VendorName() string                               { return "" }
func (s *svmUpstreamStub) NetworkId() string                                { return "svm:mainnet-beta" }
func (s *svmUpstreamStub) NetworkLabel() string                             { return "" }
func (s *svmUpstreamStub) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: "svm-stub", Type: common.UpstreamTypeSvm}
}
func (s *svmUpstreamStub) Logger() *zerolog.Logger       { l := zerolog.Nop(); return &l }
func (s *svmUpstreamStub) Vendor() common.Vendor         { return nil }
func (s *svmUpstreamStub) Tracker() common.HealthTracker { return nil }
func (s *svmUpstreamStub) Forward(context.Context, *common.NormalizedRequest, bool, bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (s *svmUpstreamStub) ShouldHandleMethod(string) (bool, error) { return true, nil }
func (s *svmUpstreamStub) Cordon(string, string)                  {}
func (s *svmUpstreamStub) Uncordon(string, string)                {}
func (s *svmUpstreamStub) IgnoreMethod(string)                    {}
func (s *svmUpstreamStub) SvmStatePoller() common.SvmStatePoller  { return s.poller }

// recordingSvmPoller captures SuggestLatestSlot calls so the test can assert
// on what the hook extracted.
type recordingSvmPoller struct {
	lastSuggested int64
}

func (r *recordingSvmPoller) Bootstrap(context.Context) error      { return nil }
func (r *recordingSvmPoller) IsObjectNull() bool                   { return false }
func (r *recordingSvmPoller) Poll(context.Context) error           { return nil }
func (r *recordingSvmPoller) LatestSlot() int64                    { return 0 }
func (r *recordingSvmPoller) FinalizedSlot() int64                 { return 0 }
func (r *recordingSvmPoller) MaxShredInsertSlotLag() int64         { return 0 }
func (r *recordingSvmPoller) IsHealthy() bool                      { return true }
func (r *recordingSvmPoller) SuggestLatestSlot(slot int64)         { r.lastSuggested = slot }
func (r *recordingSvmPoller) SuggestFinalizedSlot(slot int64)      {}

func TestUpstreamPostForward_TrackContextSlot_SuggestsFromResponse(t *testing.T) {
	t.Parallel()
	poller := &recordingSvmPoller{}
	up := &svmUpstreamStub{poller: poller}

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil,
		[]byte(`{"context":{"slot":12345,"apiVersion":"1.18"},"value":{"lamports":42}}`), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	upstreamPostForward_trackContextSlot(context.Background(), up, resp)

	if poller.lastSuggested != 12345 {
		t.Fatalf("expected SuggestLatestSlot(12345), got %d", poller.lastSuggested)
	}
}

func TestUpstreamPostForward_TrackContextSlot_IgnoresResponseWithoutContext(t *testing.T) {
	t.Parallel()
	poller := &recordingSvmPoller{}
	up := &svmUpstreamStub{poller: poller}

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100]}`))
	jrr, _ := common.NewJsonRpcResponseFromBytes(nil, []byte(`{"blockhash":"abc","parentSlot":99}`), nil)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	upstreamPostForward_trackContextSlot(context.Background(), up, resp)

	if poller.lastSuggested != 0 {
		t.Fatalf("no context.slot in response → poller must be untouched, got %d", poller.lastSuggested)
	}
}

func TestUpstreamPostForward_TrackContextSlot_NoOpForNonSvmUpstream(t *testing.T) {
	t.Parallel()
	// Plain common.Upstream with no SvmStatePoller method — hook must not panic
	// and must leave the response untouched.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))
	jrr, _ := common.NewJsonRpcResponseFromBytes(nil, []byte(`{"context":{"slot":12345}}`), nil)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	// stubSvm defined in error_normalizer_test.go satisfies common.Upstream
	// but NOT common.SvmUpstream — it has no SvmStatePoller method.
	upstreamPostForward_trackContextSlot(context.Background(), newSvmStub(), resp)
	// No assertion — test passes if it doesn't panic and returns cleanly.
}

func TestHandleUpstreamPostForward_NonSendTransaction_Unchanged(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`))
	h := &SvmArchitectureHandler{}

	upstreamErr := common.NewErrEndpointServerSideException(nil, nil, 500)
	_, err := h.HandleUpstreamPostForward(context.Background(), net, nil, req, nil, upstreamErr, false)
	if err != upstreamErr {
		t.Fatalf("non-sendTx error must pass through unchanged, got %v", err)
	}
}
