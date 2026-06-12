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
	cfg        *common.NetworkConfig
	latestSlot int64
}

func (f *fakeNetwork) Id() string                                   { return "svm:mainnet-beta" }
func (f *fakeNetwork) Label() string                                { return "" }
func (f *fakeNetwork) ProjectId() string                            { return "test" }
func (f *fakeNetwork) Architecture() common.NetworkArchitecture     { return common.ArchitectureSvm }
func (f *fakeNetwork) Config() *common.NetworkConfig                { return f.cfg }
func (f *fakeNetwork) Logger() *zerolog.Logger                      { l := zerolog.Nop(); return &l }
func (f *fakeNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (f *fakeNetwork) SvmHighestLatestSlot(context.Context) int64    { return f.latestSlot }
func (f *fakeNetwork) SvmHighestFinalizedSlot(context.Context) int64 { return 0 }
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
	methods := []string{"getBlock", "getTransaction", "getSignaturesForAddress"}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	for _, m := range methods {
		if got := GetFinality(context.Background(), net, newReq(m, "[]"), nil); got != common.DataFinalityStateFinalized {
			t.Errorf("%s: expected Finalized, got %v", m, got)
		}
	}
}

func TestFinality_ExplicitCommitment_OverridesDefault(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	// Request with commitment:finalized beats network default of confirmed.
	req := newReq("getAccountInfo", `["pubkey", {"commitment":"finalized"}]`)
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
	req := newReq("getAccountInfo", `["pubkey"]`)
	if got := GetFinality(context.Background(), net, req, nil); got != common.DataFinalityStateFinalized {
		t.Fatalf("network-level default finalized not applied: got %v", got)
	}
}

func TestFinality_NoCommitmentNoDefault_FallsBackUnfinalized(t *testing.T) {
	t.Parallel()
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	req := newReq("getAccountInfo", `["pubkey"]`)
	if got := GetFinality(context.Background(), net, req, nil); got != common.DataFinalityStateUnfinalized {
		t.Fatalf("expected safe default Unfinalized, got %v", got)
	}
}
