package svm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

// TestNetworkPreForward_InjectWriteCommitment covers the write-path normalizer:
// write/effectful methods carry commitment via their OWN config field
// (sendTransaction → preflightCommitment, simulate/airdrop → commitment) at a
// method-specific param index. It must inject the network default there, honor a
// caller-supplied value, and never corrupt a legacy shape or fabricate args.
func TestNetworkPreForward_InjectWriteCommitment(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	paramsOf := func(method, params string) []interface{} {
		req := common.NewNormalizedRequest([]byte(
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
		handled, _, err := networkPreForward_injectWriteCommitment(context.Background(), net, req)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", method, err)
		}
		if handled {
			t.Fatalf("%s: write injection must never short-circuit", method)
		}
		jrq, _ := req.JsonRpcRequest(context.Background())
		return jrq.Params
	}

	// sendTransaction: config object appended at index 1 with preflightCommitment
	// (NOT "commitment").
	p := paramsOf("sendTransaction", `["base64tx"]`)
	if len(p) != 2 {
		t.Fatalf("sendTransaction: expected config appended at idx 1, got %+v", p)
	}
	if m, ok := p[1].(map[string]interface{}); !ok || m["preflightCommitment"] != "confirmed" || m["commitment"] != nil {
		t.Errorf("sendTransaction: expected preflightCommitment:confirmed (no commitment), got %+v", p[1])
	}

	// sendTransaction with an existing config object: field merged in.
	p = paramsOf("sendTransaction", `["base64tx", {"skipPreflight":true}]`)
	if m, ok := p[1].(map[string]interface{}); !ok || m["preflightCommitment"] != "confirmed" || m["skipPreflight"] != true {
		t.Errorf("sendTransaction merge: expected preflightCommitment added beside skipPreflight, got %+v", p[1])
	}

	// Caller-supplied preflightCommitment must win.
	p = paramsOf("sendTransaction", `["base64tx", {"preflightCommitment":"finalized"}]`)
	if m := p[1].(map[string]interface{}); m["preflightCommitment"] != "finalized" {
		t.Errorf("sendTransaction: caller's preflightCommitment must win, got %v", m["preflightCommitment"])
	}

	// simulateTransaction uses "commitment", appended at index 1.
	p = paramsOf("simulateTransaction", `["base64tx"]`)
	if m, ok := p[1].(map[string]interface{}); !ok || m["commitment"] != "confirmed" || m["preflightCommitment"] != nil {
		t.Errorf("simulateTransaction: expected commitment:confirmed at idx 1, got %+v", p[1])
	}

	// requestAirdrop: config object lives at index 2 (after pubkey + lamports),
	// field "commitment". Missing lamports → must NOT fabricate args.
	p = paramsOf("requestAirdrop", `["pubkey", 1000000000]`)
	if len(p) != 3 {
		t.Fatalf("requestAirdrop: expected config appended at idx 2, got %+v", p)
	}
	if m, ok := p[2].(map[string]interface{}); !ok || m["commitment"] != "confirmed" {
		t.Errorf("requestAirdrop: expected commitment:confirmed at idx 2, got %+v", p[2])
	}
	if p := paramsOf("requestAirdrop", `["pubkey"]`); len(p) != 1 {
		t.Errorf("requestAirdrop missing lamports: params must stay untouched, got %+v", p)
	}

	// No network default configured → nothing injected.
	bare := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["base64tx"]}`))
	if _, _, err := networkPreForward_injectWriteCommitment(context.Background(), bare, req); err != nil {
		t.Fatalf("no-default: unexpected error: %v", err)
	}
	if jrq, _ := req.JsonRpcRequest(context.Background()); len(jrq.Params) != 1 {
		t.Errorf("no-default: params must stay untouched, got %+v", jrq.Params)
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

	// getBlocks with EMPTY params is missing its required start slot — injection
	// must NOT append an options object (which would put {commitment} where the
	// start slot belongs). Leave it for the upstream to reject.
	if p := paramsOf("getBlocks", `[]`); len(p) != 0 {
		t.Errorf("getBlocks []: expected params untouched (missing start slot), got %+v", p)
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

func (s *svmUpstreamStub) Id() string           { return "svm-stub" }
func (s *svmUpstreamStub) VendorName() string   { return "" }
func (s *svmUpstreamStub) NetworkId() string    { return "svm:mainnet-beta" }
func (s *svmUpstreamStub) NetworkLabel() string { return "" }
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
func (s *svmUpstreamStub) Cordon(string, string)                   {}
func (s *svmUpstreamStub) Uncordon(string, string)                 {}
func (s *svmUpstreamStub) IgnoreMethod(string)                     {}
func (s *svmUpstreamStub) SvmStatePoller() common.SvmStatePoller   { return s.poller }

// recordingSvmPoller captures SuggestLatestSlot / SuggestFinalizedSlot calls so
// the test can assert on what the hook extracted and how it routed it.
type recordingSvmPoller struct {
	lastSuggested          int64
	lastFinalizedSuggested int64
}

func (r *recordingSvmPoller) Bootstrap(context.Context) error   { return nil }
func (r *recordingSvmPoller) IsObjectNull() bool                { return false }
func (r *recordingSvmPoller) Poll(context.Context) error        { return nil }
func (r *recordingSvmPoller) LatestSlot() int64                 { return 0 }
func (r *recordingSvmPoller) FinalizedSlot() int64              { return 0 }
func (r *recordingSvmPoller) ShredInsertSlot() int64            { return 0 }
func (r *recordingSvmPoller) MaxShredInsertSlotLag() int64      { return 0 }
func (r *recordingSvmPoller) IsHealthy() bool                   { return true }
func (r *recordingSvmPoller) SuggestLatestSlot(slot int64)      { r.lastSuggested = slot }
func (r *recordingSvmPoller) SuggestFinalizedSlot(slot int64)   { r.lastFinalizedSuggested = slot }
func (r *recordingSvmPoller) SetPollInterval(time.Duration)     {}
func (r *recordingSvmPoller) SetDebounceInterval(time.Duration) {}

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

	upstreamPostForward_trackContextSlot(context.Background(), nil, up, req, resp)

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

	upstreamPostForward_trackContextSlot(context.Background(), nil, up, req, resp)

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
	upstreamPostForward_trackContextSlot(context.Background(), nil, newSvmStub(), req, resp)
	// No assertion — test passes if it doesn't panic and returns cleanly.
}

// TestUpstreamPostForward_TrackContextSlot_CommitmentRouting locks the
// commitment-routed harvesting contract: context.slot on a response whose
// EFFECTIVE commitment (explicit param wins, else network default) is
// "finalized" feeds BOTH the finalized and latest views; any weaker
// commitment — or a nil network, which makes the default unresolvable —
// feeds only the latest view.
func TestUpstreamPostForward_TrackContextSlot_CommitmentRouting(t *testing.T) {
	t.Parallel()

	const slot = int64(98765)
	confirmedNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	finalizedNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}

	cases := []struct {
		name          string
		network       common.Network
		reqBody       string
		wantFinalized int64
	}{
		{
			// Explicit param beats the weaker network default.
			name:          "explicit finalized feeds both views",
			network:       confirmedNet,
			reqBody:       `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"commitment":"finalized"}]}`,
			wantFinalized: slot,
		},
		{
			// Explicit param beats the stronger network default too.
			name:          "explicit confirmed feeds latest only",
			network:       finalizedNet,
			reqBody:       `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"commitment":"confirmed"}]}`,
			wantFinalized: 0,
		},
		{
			name:          "network default finalized feeds both views",
			network:       finalizedNet,
			reqBody:       `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey"]}`,
			wantFinalized: slot,
		},
		{
			// Without a network the effective commitment is unknowable, so
			// even an explicit finalized param must NOT feed the finalized
			// view — locks the n != nil guard.
			name:          "nil network feeds latest only",
			network:       nil,
			reqBody:       `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pubkey",{"commitment":"finalized"}]}`,
			wantFinalized: 0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			poller := &recordingSvmPoller{}
			up := &svmUpstreamStub{poller: poller}

			req := common.NewNormalizedRequest([]byte(tc.reqBody))
			jrr, err := common.NewJsonRpcResponseFromBytes(nil,
				[]byte(fmt.Sprintf(`{"context":{"slot":%d,"apiVersion":"1.18"},"value":{"lamports":42}}`, slot)), nil)
			if err != nil {
				t.Fatalf("build response: %v", err)
			}
			resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

			upstreamPostForward_trackContextSlot(context.Background(), tc.network, up, req, resp)

			if poller.lastSuggested != slot {
				t.Fatalf("expected SuggestLatestSlot(%d), got %d", slot, poller.lastSuggested)
			}
			if poller.lastFinalizedSuggested != tc.wantFinalized {
				t.Fatalf("expected SuggestFinalizedSlot(%d), got %d", tc.wantFinalized, poller.lastFinalizedSuggested)
			}
		})
	}
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

func slotResponse(t *testing.T, method string, slot int64) (*common.NormalizedRequest, *common.NormalizedResponse) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)
	req := common.NewNormalizedRequest([]byte(body))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(fmt.Sprintf("%d", slot)), nil)
	if err != nil {
		t.Fatalf("build slot response: %v", err)
	}
	return req, common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func readSlot(t *testing.T, resp *common.NormalizedResponse) int64 {
	t.Helper()
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var slot int64
	if err := json.Unmarshal(jrr.GetResultBytes(), &slot); err != nil {
		t.Fatalf("unmarshal slot: %v", err)
	}
	return slot
}

func TestNetworkPostForward_GetSlot_UpgradesStaleResponse(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 12345678}
	req, resp := slotResponse(t, "getSlot", 12340000)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot := readSlot(t, got); slot != 12345678 {
		t.Fatalf("expected upgraded slot 12345678, got %d", slot)
	}
}

func TestNetworkPostForward_GetSlot_AlreadyAtTip_Unchanged(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 12340000}
	req, resp := slotResponse(t, "getSlot", 12340000)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resp {
		t.Fatal("response at tip must be returned unchanged (same pointer)")
	}
}

func TestNetworkPostForward_GetSlot_ErrorPassthrough(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 9999}
	req, _ := slotResponse(t, "getSlot", 0)
	upstreamErr := common.NewErrEndpointServerSideException(nil, nil, 500)

	got, err := networkPostForward_getSlot(context.Background(), net, req, nil, upstreamErr)
	if err != upstreamErr {
		t.Fatalf("error must pass through unchanged, got %v", err)
	}
	if got != nil {
		t.Fatal("resp must be nil when error present")
	}
}

func TestNetworkPostForward_GetSlot_FromCachePreserved(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 12345678}
	req, resp := slotResponse(t, "getSlot", 12340000)
	resp.WithFromCache(true)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.FromCache() {
		t.Fatal("corrected response must preserve FromCache=true")
	}
}

// TestNetworkPostForward_GetSlot_FinalizedCommitment_UsesFinalizeTip verifies
// that a stale getSlot(finalized) response is corrected using the finalized tip
// minus the indexing lag (not the raw tip, not the processed tip).
// finalizedSlot=12345000, lag=32 → floor=12344968; upstream returned 12340000.
func TestNetworkPostForward_GetSlot_FinalizedCommitment_UsesFinalizeTip(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}, latestSlot: 12345678, finalizedSlot: 12345000}

	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`
	req := common.NewNormalizedRequest([]byte(body))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte("12340000"), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Floor = 12345000 - 32 = 12344968. 12340000 < 12344968 so must be upgraded.
	// Must NOT be upgraded to raw tip (12345000) which may not be indexed yet.
	if slot := readSlot(t, got); slot != 12344968 {
		t.Fatalf("expected floor 12344968 (finalizedTip-lag), got %d", slot)
	}
}

// TestNetworkPostForward_GetSlot_FinalizedCommitment_CapsAboveFloor verifies
// that a fresh finalized response above the indexing-lag floor is capped down
// to the floor. finalizedSlot=12345000, lag=32 → floor=12344968; upstream
// returned 12344990 (above floor, block may not be indexed) → capped to 12344968.
func TestNetworkPostForward_GetSlot_FinalizedCommitment_CapsAboveFloor(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}, latestSlot: 12345678, finalizedSlot: 12345000}

	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`
	req := common.NewNormalizedRequest([]byte(body))
	// 12344990 > floor (12344968): above the safe window, must be capped down.
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte("12344990"), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot := readSlot(t, got); slot != 12344968 {
		t.Fatalf("expected floor 12344968 (cap from above), got %d", slot)
	}
}

// TestNetworkPostForward_GetSlot_FinalizedCommitment_NoOverrideAtFloor verifies
// that a finalized response exactly at the floor is returned unchanged.
func TestNetworkPostForward_GetSlot_FinalizedCommitment_NoOverrideAtFloor(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}, latestSlot: 12345678, finalizedSlot: 12345000}

	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`
	req := common.NewNormalizedRequest([]byte(body))
	// Exactly at floor (12344968) — pass through unchanged.
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte("12344968"), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != resp {
		t.Fatalf("response exactly at floor must be returned unchanged; got slot %d", readSlot(t, got))
	}
}

// TestNetworkPostForward_GetSlot_FinalizedCommitment_AppliesLagWhenTipUnknown verifies
// that when SvmHighestFinalizedSlot is 0 (state poller not populated, e.g. devnet
// upstreams rate-limiting it), the hook applies the indexing lag to the response
// itself rather than passing the raw consensus tip through. Passing through the
// raw tip causes getBlock to return -32004 because the block isn't indexed yet.
func TestNetworkPostForward_GetSlot_FinalizedCommitment_AppliesLagWhenTipUnknown(t *testing.T) {
	t.Parallel()
	// finalizedSlot=0 simulates a pod where the state poller never fires.
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}, latestSlot: 0, finalizedSlot: 0}

	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`
	req := common.NewNormalizedRequest([]byte(body))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte("476080663"), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect slotNumber - 32 = 476080631, not the raw tip 476080663.
	if slot := readSlot(t, got); slot != 476080631 {
		t.Fatalf("expected lag-adjusted slot 476080631, got %d", slot)
	}
}

// TestNetworkPostForward_GetSlot_FinalizedCommitment_UsesIndexedSlotWhenBehind
// verifies that when the provider's shred-insert slot is behind the finalized
// consensus tip, the hook uses the shred-insert slot as the ceiling rather than
// finalizedTip - 32. This is the devnet scenario: the indexer is slow and the
// fixed-lag fallback (32 slots) would still be too optimistic.
func TestNetworkPostForward_GetSlot_FinalizedCommitment_UsesIndexedSlotWhenBehind(t *testing.T) {
	t.Parallel()
	// finalizedTip=12345000, indexedSlot=12344500 (500 slots behind — worse than -32).
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}, latestSlot: 12345678, finalizedSlot: 12345000, indexedSlot: 12344500}

	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`
	req := common.NewNormalizedRequest([]byte(body))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte("12345000"), nil)
	if err != nil {
		t.Fatalf("build response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Floor = min(12345000, 12344500) = 12344500 (not finalizedTip-32 = 12344968).
	if slot := readSlot(t, got); slot != 12344500 {
		t.Fatalf("expected indexed floor 12344500 (shred insert slot), got %d", slot)
	}
}

// networkPreForward_getBlock: slot beyond indexedTip + staleness margin →
// short-circuit missing-data. fakeNetwork's config carries no Svm section, so
// the margin is the floor of 2 slots: tip 1000 → first rejected slot is 1003.
func TestNetworkPreForwardGetBlock_SlotBeyondStalenessMargin_ShortCircuits(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg:         &common.NetworkConfig{Architecture: common.ArchitectureSvm},
		indexedSlot: 1000,
	}
	for _, method := range []string{"getBlock", "getConfirmedBlock"} {
		req := newReq(method, `[1003, {"encoding":"jsonParsed"}]`)
		handled, resp, err := networkPreForward_getBlock(context.Background(), net, req)
		if !handled {
			t.Fatalf("%s: expected short-circuit for slot 1003 > indexedTip 1000 + margin 2", method)
		}
		if resp != nil {
			t.Fatalf("%s: expected nil response on short-circuit", method)
		}
		if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			t.Fatalf("%s: expected ErrEndpointMissingData, got %T: %v", method, err, err)
		}
		// Verify wire code is -32014 so sol-client maps it to BlockNotAvailableException.
		// Without ErrJsonRpcExceptionInternal in the chain, TranslateToJsonRpcException
		// falls through to -32603 (ServerSideException) which sol-client doesn't handle.
		if !common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal) {
			t.Fatalf("%s: guard error must carry ErrJsonRpcExceptionInternal for correct wire code, got %T: %v", method, err, err)
		}
	}
}

// slot at/below indexedTip, and up to the staleness margin above it, → pass
// through. The 1-2-slots-ahead rows pin the false-reject fix: a live confirmed
// head is routinely 1-2 slots above the poll-debounce-stale frontier snapshot.
func TestNetworkPreForwardGetBlock_SlotWithinStalenessMargin_PassesThrough(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg:         &common.NetworkConfig{Architecture: common.ArchitectureSvm},
		indexedSlot: 1000,
	}
	for _, method := range []string{"getBlock", "getConfirmedBlock"} {
		for _, slot := range []int64{999, 1000, 1001, 1002} {
			req := newReq(method, fmt.Sprintf(`[%d]`, slot))
			handled, _, err := networkPreForward_getBlock(context.Background(), net, req)
			if handled || err != nil {
				t.Fatalf("%s slot %d: expected pass-through (handled=%v err=%v)", method, slot, handled, err)
			}
		}
	}
}

// indexedTip unavailable → falls back to finalizedTip with the same margin.
func TestNetworkPreForwardGetBlock_NoIndexedTip_FallsBackToFinalizedTip(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm},
		indexedSlot:   0, // unavailable
		finalizedSlot: 1000,
	}
	// slot beyond finalizedTip + margin 2 → short-circuit
	req := newReq("getBlock", `[1003]`)
	handled, _, err := networkPreForward_getBlock(context.Background(), net, req)
	if !handled || !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected short-circuit via finalizedTip fallback, got handled=%v err=%v", handled, err)
	}

	// slot within finalizedTip + margin → pass through
	req2 := newReq("getBlock", `[1002]`)
	handled2, _, err2 := networkPreForward_getBlock(context.Background(), net, req2)
	if handled2 || err2 != nil {
		t.Fatalf("slot 1002 within finalizedTip 1000 + margin 2: expected pass-through, got handled=%v err=%v", handled2, err2)
	}
}

// both tips unavailable (cold poller) → pass through unconditionally.
func TestNetworkPreForwardGetBlock_BothTipsZero_PassesThrough(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req := newReq("getBlock", `[999999]`)
	handled, _, err := networkPreForward_getBlock(context.Background(), net, req)
	if handled || err != nil {
		t.Fatalf("cold poller: expected pass-through for any slot, got handled=%v err=%v", handled, err)
	}
}

// handler dispatches getBlock and getConfirmedBlock to the guard.
func TestHandleNetworkPreForward_DispatchesGetBlockGuard(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg:         &common.NetworkConfig{Architecture: common.ArchitectureSvm},
		indexedSlot: 500,
	}
	h := &SvmArchitectureHandler{}
	for _, method := range []string{"getBlock", "getConfirmedBlock"} {
		req := newReq(method, `[503]`) // tip 500 + margin 2 → first rejected slot
		handled, _, err := h.HandleNetworkPreForward(context.Background(), net, nil, req)
		if !handled || !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			t.Fatalf("%s: expected dispatch to guard, got handled=%v err=%v", method, handled, err)
		}
	}
}

// enforceBlockAvailability:false → guard disabled, slot ahead of tip passes through.
func TestNetworkPreForwardGetBlock_GuardDisabled_PassesThrough(t *testing.T) {
	t.Parallel()
	f := false
	net := &fakeNetwork{
		cfg:                      &common.NetworkConfig{Architecture: common.ArchitectureSvm},
		indexedSlot:              1000,
		enforceBlockAvailability: &f,
	}
	req := newReq("getBlock", `[9999]`)
	handled, _, err := networkPreForward_getBlock(context.Background(), net, req)
	if handled || err != nil {
		t.Fatalf("guard disabled: expected pass-through for slot 9999, got handled=%v err=%v", handled, err)
	}
}

// margin widens with the configured StatePollerInterval: 2s → 2+floor(2000/400) = 7.
func TestNetworkPreForwardGetBlock_MarginDerivesFromInterval(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{StatePollerInterval: common.Duration(2 * time.Second)},
		},
		indexedSlot: 1000,
	}
	// tip+7 → within margin, forwards
	req := newReq("getBlock", `[1007]`)
	handled, _, err := networkPreForward_getBlock(context.Background(), net, req)
	if handled || err != nil {
		t.Fatalf("slot 1007 within tip 1000 + margin 7: expected pass-through, got handled=%v err=%v", handled, err)
	}
	// tip+8 → beyond margin, short-circuits
	req2 := newReq("getBlock", `[1008]`)
	handled2, _, err2 := networkPreForward_getBlock(context.Background(), net, req2)
	if !handled2 || !common.HasErrorCode(err2, common.ErrCodeEndpointMissingData) {
		t.Fatalf("slot 1008 beyond tip 1000 + margin 7: expected short-circuit, got handled=%v err=%v", handled2, err2)
	}
}

// indexedTipStalenessMargin: 2 base slots + floor(interval / 400ms).
func TestIndexedTipStalenessMargin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  *common.NetworkConfig
		want int64
	}{
		{"nil config", nil, 2},
		{"no svm section", &common.NetworkConfig{Architecture: common.ArchitectureSvm}, 2},
		{"svm without interval", &common.NetworkConfig{Svm: &common.SvmNetworkConfig{}}, 2},
		{"400ms interval", &common.NetworkConfig{Svm: &common.SvmNetworkConfig{StatePollerInterval: common.Duration(400 * time.Millisecond)}}, 3},
		{"1s interval", &common.NetworkConfig{Svm: &common.SvmNetworkConfig{StatePollerInterval: common.Duration(time.Second)}}, 4},
		{"2s interval", &common.NetworkConfig{Svm: &common.SvmNetworkConfig{StatePollerInterval: common.Duration(2 * time.Second)}}, 7},
		{"5s interval", &common.NetworkConfig{Svm: &common.SvmNetworkConfig{StatePollerInterval: common.Duration(5 * time.Second)}}, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexedTipStalenessMargin(&fakeNetwork{cfg: tc.cfg}); got != tc.want {
				t.Fatalf("margin = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHandleNetworkPostForward_DispatchesGetSlotAndGetBlockHeight(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 99999}
	h := &SvmArchitectureHandler{}
	for _, method := range []string{"getSlot", "getBlockHeight", "GETSLOT", "GetBlockHeight"} {
		req, resp := slotResponse(t, method, 12345)
		got, err := h.HandleNetworkPostForward(context.Background(), net, req, resp, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", method, err)
		}
		if slot := readSlot(t, got); slot != 99999 {
			t.Fatalf("%s: expected upgraded slot 99999, got %d", method, slot)
		}
	}
}
