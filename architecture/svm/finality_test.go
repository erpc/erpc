package svm

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// fakeNetwork satisfies common.SvmNetwork with only the methods SVM code
// paths touch. EVM accessors don't need to be implemented anymore — they're
// behind common.EvmNetwork, which this type deliberately does not satisfy.
type fakeNetwork struct {
	cfg                      *common.NetworkConfig
	latestSlot               int64
	finalizedSlot            int64
	indexedSlot              int64
	enforceBlockAvailability *bool
}

func (f *fakeNetwork) Id() string                                    { return "svm:mainnet-beta" }
func (f *fakeNetwork) Label() string                                 { return "" }
func (f *fakeNetwork) ProjectId() string                             { return "test" }
func (f *fakeNetwork) Architecture() common.NetworkArchitecture      { return common.ArchitectureSvm }
func (f *fakeNetwork) Config() *common.NetworkConfig                 { return f.cfg }
func (f *fakeNetwork) Logger() *zerolog.Logger                       { l := zerolog.Nop(); return &l }
func (f *fakeNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (f *fakeNetwork) SvmHighestLatestSlot(context.Context) int64    { return f.latestSlot }
func (f *fakeNetwork) SvmHighestFinalizedSlot(context.Context) int64 { return f.finalizedSlot }
func (f *fakeNetwork) SvmHighestIndexedSlot(context.Context) int64   { return f.indexedSlot }
func (f *fakeNetwork) SvmEnforceBlockAvailability() bool {
	if f.enforceBlockAvailability == nil {
		return true
	}
	return *f.enforceBlockAvailability
}
func (f *fakeNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (f *fakeNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return GetFinality(ctx, f, req, resp)
}

func newReq(method, paramsJson string) *common.NormalizedRequest {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, paramsJson)
	return common.NewNormalizedRequest([]byte(body))
}

func TestFinality_NeverCacheMethods_ReturnRealtime(t *testing.T) {
	t.Parallel()
	methods := []string{"getLatestBlockhash", "sendTransaction", "simulateTransaction", "getSignatureStatuses"}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	for _, m := range methods {
		if got := GetFinality(context.Background(), net, newReq(m, "[]"), nil); got != common.DataFinalityStateRealtime {
			t.Errorf("%s: expected Realtime, got %v", m, got)
		}
	}
}

func TestFinality_AlwaysFinalizedMethods_ReturnFinalized(t *testing.T) {
	t.Parallel()
	// Only methods finalized by construction (no commitment param) belong here.
	methods := []string{"getInflationReward", "getBlockTime"}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	for _, m := range methods {
		if got := GetFinality(context.Background(), net, newReq(m, "[]"), nil); got != common.DataFinalityStateFinalized {
			t.Errorf("%s: expected Finalized, got %v", m, got)
		}
	}
}

// Regression for the finality misclassification fix: getBlock / getTransaction
// honor the request's commitment and can return confirmed (not-yet-rooted)
// data, so they must NOT be hardcoded as finalized — a confirmed response must
// classify as Unfinalized (re-org aware), and only an explicit/effective
// finalized commitment promotes them.
func TestFinality_CommitmentSensitiveMethods_NotAlwaysFinalized(t *testing.T) {
	t.Parallel()
	// Only slot/signature-pinned reads can ever be promoted; see slotPinnedMethods.
	methods := []string{"getBlock", "getTransaction"}

	confirmedNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	for _, m := range methods {
		// Explicit confirmed → unfinalized.
		req := newReq(m, `[{"commitment":"confirmed"}]`)
		if got := GetFinality(context.Background(), confirmedNet, req, nil); got != common.DataFinalityStateUnfinalized {
			t.Errorf("%s confirmed: expected Unfinalized, got %v", m, got)
		}
		// No explicit commitment + confirmed network default → unfinalized.
		if got := GetFinality(context.Background(), confirmedNet, newReq(m, "[]"), nil); got != common.DataFinalityStateUnfinalized {
			t.Errorf("%s default-confirmed: expected Unfinalized, got %v", m, got)
		}
		// Explicit finalized → finalized.
		reqF := newReq(m, `[{"commitment":"finalized"}]`)
		if got := GetFinality(context.Background(), confirmedNet, reqF, nil); got != common.DataFinalityStateFinalized {
			t.Errorf("%s finalized: expected Finalized, got %v", m, got)
		}
	}
}

// TestFinality_MovingHeadReadsNeverFinalized is the regression guard for the
// stale-forever bug: Solana's `finalized` commitment is the state at the latest
// ROOTED slot, a head that advances every ~400ms, so a state read at finalized
// is NOT immutable. Classifying it Finalized (the zero value, which a policy
// with no explicit finality matches) combined with an unset TTL (= no expiry in
// the connectors) pinned e.g. getBalance to its first observed value forever.
// These must all be Realtime — the same treatment EVM gives the `latest` tag.
func TestFinality_MovingHeadReadsNeverFinalized(t *testing.T) {
	t.Parallel()
	finalizedNet := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}

	// Account/state reads: the answer changes with every transfer.
	stateReads := map[string]string{
		"getBalance":             `["pubkey", {"commitment":"finalized"}]`,
		"getAccountInfo":         `["pubkey", {"commitment":"finalized"}]`,
		"getMultipleAccounts":    `[["pubkey"], {"commitment":"finalized"}]`,
		"getProgramAccounts":     `["program", {"commitment":"finalized"}]`,
		"getTokenAccountBalance": `["pubkey", {"commitment":"finalized"}]`,
		"getTokenSupply":         `["mint", {"commitment":"finalized"}]`,
		"getSupply":              `[{"commitment":"finalized"}]`,
		"getSlot":                `[{"commitment":"finalized"}]`,
		"getBlockHeight":         `[{"commitment":"finalized"}]`,
		"getTransactionCount":    `[{"commitment":"finalized"}]`,
		// Range/list reads whose upper bound tracks the head, so they grow.
		"getSignaturesForAddress": `["pubkey", {"commitment":"finalized"}]`,
		"getBlocks":               `[100, {"commitment":"finalized"}]`,
		"getBlocksWithLimit":      `[100, 10, {"commitment":"finalized"}]`,
	}
	for m, params := range stateReads {
		if got := GetFinality(context.Background(), finalizedNet, newReq(m, params), nil); got != common.DataFinalityStateRealtime {
			t.Errorf("%s at commitment=finalized: expected Realtime (moving head), got %v", m, got)
		}
	}

	// minContextSlot is a node-freshness floor, not a history bound — it must
	// not promote a moving-head read either.
	pinned := newReq("getBalance", `["pubkey", {"commitment":"finalized","minContextSlot":12345}]`)
	if got := GetFinality(context.Background(), finalizedNet, pinned, nil); got != common.DataFinalityStateRealtime {
		t.Errorf("getBalance with minContextSlot: expected Realtime, got %v", got)
	}

	// …while a genuinely slot-addressed read at finalized stays cacheable.
	// This is the whole value of the cache for the ETL workload.
	if got := GetFinality(context.Background(), finalizedNet, newReq("getBlock", `[100, {"commitment":"finalized"}]`), nil); got != common.DataFinalityStateFinalized {
		t.Errorf("getBlock(slot) at commitment=finalized: expected Finalized, got %v", got)
	}
	if got := GetFinality(context.Background(), finalizedNet, newReq("getBlockTime", `[100]`), nil); got != common.DataFinalityStateFinalized {
		t.Errorf("getBlockTime(slot): expected Finalized, got %v", got)
	}
}

func TestFinality_ExplicitCommitment_OverridesDefault(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	// Request with commitment:finalized beats network default of confirmed.
	// Uses a slot-pinned method: only those are promotable at all, so this is
	// where the override is observable.
	req := newReq("getTransaction", `["sig", {"commitment":"finalized"}]`)
	if got := GetFinality(context.Background(), net, req, nil); got != common.DataFinalityStateFinalized {
		t.Fatalf("explicit finalized commitment not honored: got %v", got)
	}
}

func TestFinality_NetworkDefaultCommitment_Applies(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	req := newReq("getTransaction", `["sig"]`)
	if got := GetFinality(context.Background(), net, req, nil); got != common.DataFinalityStateFinalized {
		t.Fatalf("network-level default finalized not applied: got %v", got)
	}
}

// TestFinality_DefaultNotTrustedWhenInjectionSkips guards the rule that finality
// reflects the commitment that ACTUALLY reaches the upstream, not merely whether
// a network default exists. When commitment injection legitimately skips a
// request, the network default must NOT promote it to Finalized.
func TestFinality_DefaultNotTrustedWhenInjectionSkips(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}

	// Legacy getBlock(slot,"base64"): options slot is a non-object string, so
	// injection skips → the default never reaches the upstream → Unfinalized
	// (NOT promoted to Finalized by the network default).
	if got := GetFinality(context.Background(), net, newReq("getBlock", `[100, "base64"]`), nil); got != common.DataFinalityStateUnfinalized {
		t.Errorf("legacy getBlock (injection skipped): expected Unfinalized, got %v", got)
	}
	// Object options form → default finalized is injected → Finalized.
	if got := GetFinality(context.Background(), net, newReq("getBlock", `[100, {}]`), nil); got != common.DataFinalityStateFinalized {
		t.Errorf("getBlock object form: expected Finalized, got %v", got)
	}
	// Bare [slot] → options appended with default finalized → Finalized.
	if got := GetFinality(context.Background(), net, newReq("getBlock", `[100]`), nil); got != common.DataFinalityStateFinalized {
		t.Errorf("getBlock bare slot: expected Finalized, got %v", got)
	}
	// Explicit confirmed beats the default.
	if got := GetFinality(context.Background(), net, newReq("getBlock", `[100, {"commitment":"confirmed"}]`), nil); got != common.DataFinalityStateUnfinalized {
		t.Errorf("explicit confirmed: expected Unfinalized, got %v", got)
	}
}

func TestFinality_NoCommitmentNoDefault_FallsBackUnfinalized(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	// Slot-pinned but no resolvable commitment → Unfinalized (fork-droppable).
	if got := GetFinality(context.Background(), net, newReq("getBlock", `[100]`), nil); got != common.DataFinalityStateUnfinalized {
		t.Fatalf("expected safe default Unfinalized for getBlock, got %v", got)
	}
	// Moving-head read → Realtime regardless of commitment resolution.
	if got := GetFinality(context.Background(), net, newReq("getAccountInfo", `["pubkey"]`), nil); got != common.DataFinalityStateRealtime {
		t.Fatalf("expected Realtime for getAccountInfo, got %v", got)
	}
}

// TestIsFinalizedCommitment_IsNotGetFinality pins the distinction the two
// functions now encode: for a moving-head read at commitment:finalized the
// RESPONSE is not cacheable-as-final (GetFinality → Realtime) while the
// REQUEST still evaluates at the rooted slot (IsFinalizedCommitment → true).
// Upstream routing needs the second answer; the cache needs the first.
func TestIsFinalizedCommitment_IsNotGetFinality(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
	}}
	ctx := context.Background()

	movingHead := newReq("getBalance", `["pubkey", {"commitment":"finalized","minContextSlot":900}]`)
	if got := GetFinality(ctx, net, movingHead, nil); got != common.DataFinalityStateRealtime {
		t.Errorf("getBalance cacheability: expected Realtime, got %v", got)
	}
	if !IsFinalizedCommitment(ctx, net, movingHead) {
		t.Error("getBalance routing: expected finalized commitment (compare against upstream FinalizedSlot)")
	}

	// Explicit weaker commitment beats the network default for routing too.
	confirmed := newReq("getBalance", `["pubkey", {"commitment":"confirmed","minContextSlot":900}]`)
	if IsFinalizedCommitment(ctx, net, confirmed) {
		t.Error("explicit confirmed must not report a finalized commitment")
	}

	// Injection skipped (legacy encoding-string form) → no default reaches the
	// upstream, so routing must not assume finalized either.
	if IsFinalizedCommitment(ctx, net, newReq("getBlock", `[100, "base64"]`)) {
		t.Error("injection-skipped request must not report a finalized commitment")
	}
	if IsFinalizedCommitment(ctx, net, nil) {
		t.Error("nil request must not report a finalized commitment")
	}
}
