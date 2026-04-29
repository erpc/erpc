package auth

import (
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
)

func newPaymentRequired(network, asset string) *common.ErrPaymentRequired {
	resp := X402PaymentRequirementsResponse{
		X402Version: 2,
		Error:       "Payment required for this resource",
		Accepts: []X402PaymentRequirement{
			{Scheme: "exact", Network: network, Asset: asset, Amount: "5", PayTo: "0xSeller"},
		},
	}
	err := common.NewErrPaymentRequired(resp)
	var pe *common.ErrPaymentRequired
	if !errors.As(err, &pe) {
		panic("expected *common.ErrPaymentRequired")
	}
	return pe
}

func TestMergePaymentRequired_SingleErrorPassesThrough(t *testing.T) {
	in := newPaymentRequired("eip155:8453", "0xUSDC-base")
	got := mergePaymentRequired([]*common.ErrPaymentRequired{in})

	var pe *common.ErrPaymentRequired
	if !errors.As(got, &pe) {
		t.Fatalf("expected *common.ErrPaymentRequired, got %T", got)
	}
	resp, ok := pe.PaymentRequirements.(X402PaymentRequirementsResponse)
	if !ok {
		t.Fatalf("expected X402PaymentRequirementsResponse, got %T", pe.PaymentRequirements)
	}
	if len(resp.Accepts) != 1 {
		t.Fatalf("Accepts: want 1, got %d", len(resp.Accepts))
	}
	if resp.Accepts[0].Network != "eip155:8453" {
		t.Errorf("Network: want eip155:8453, got %q", resp.Accepts[0].Network)
	}
}

func TestMergePaymentRequired_MultipleErrorsConcatAccepts(t *testing.T) {
	errs := []*common.ErrPaymentRequired{
		newPaymentRequired("eip155:8453", "0xUSDC-base"),
		newPaymentRequired("eip155:1", "0xUSDC-eth"),
		newPaymentRequired("eip155:42161", "0xUSDC-arb"),
	}
	got := mergePaymentRequired(errs)

	var pe *common.ErrPaymentRequired
	if !errors.As(got, &pe) {
		t.Fatalf("expected *common.ErrPaymentRequired, got %T", got)
	}
	resp, ok := pe.PaymentRequirements.(X402PaymentRequirementsResponse)
	if !ok {
		t.Fatalf("expected X402PaymentRequirementsResponse, got %T", pe.PaymentRequirements)
	}
	if len(resp.Accepts) != 3 {
		t.Fatalf("Accepts: want 3, got %d", len(resp.Accepts))
	}
	wantNetworks := []string{"eip155:8453", "eip155:1", "eip155:42161"}
	for i, want := range wantNetworks {
		if resp.Accepts[i].Network != want {
			t.Errorf("Accepts[%d].Network: want %q, got %q", i, want, resp.Accepts[i].Network)
		}
	}
}

func TestMergePaymentRequired_NonX402PayloadFallsBackToFirst(t *testing.T) {
	// First entry is well-formed x402; second carries a foreign payload.
	first := newPaymentRequired("eip155:8453", "0xUSDC-base")
	foreignErr := common.NewErrPaymentRequired(map[string]string{"scheme": "future-non-x402"})
	var foreign *common.ErrPaymentRequired
	if !errors.As(foreignErr, &foreign) {
		t.Fatalf("expected *common.ErrPaymentRequired")
	}

	got := mergePaymentRequired([]*common.ErrPaymentRequired{first, foreign})

	var pe *common.ErrPaymentRequired
	if !errors.As(got, &pe) {
		t.Fatalf("expected *common.ErrPaymentRequired, got %T", got)
	}
	// Should be the first error verbatim, not a merged response.
	if pe != first {
		t.Errorf("expected first error verbatim on type-assertion failure")
	}
}
