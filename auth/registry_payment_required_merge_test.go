package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
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

func TestMergePaymentRequired_PreservesFirstResponseHeaderFields(t *testing.T) {
	first := X402PaymentRequirementsResponse{
		X402Version: 2,
		Error:       "Payment required for this resource",
		Accepts:     []X402PaymentRequirement{{Scheme: "exact", Network: "eip155:8453"}},
		Resource:    map[string]string{"url": "https://edge.test/standard/evm/1"},
	}
	second := X402PaymentRequirementsResponse{
		X402Version: 2,
		Error:       "different error string that should be ignored",
		Accepts:     []X402PaymentRequirement{{Scheme: "exact", Network: "eip155:1"}},
		Resource:    map[string]string{"url": "https://different.example/"},
	}
	wrap := func(r X402PaymentRequirementsResponse) *common.ErrPaymentRequired {
		err := common.NewErrPaymentRequired(r)
		var pe *common.ErrPaymentRequired
		errors.As(err, &pe)
		return pe
	}

	got := mergePaymentRequired([]*common.ErrPaymentRequired{wrap(first), wrap(second)})

	var pe *common.ErrPaymentRequired
	if !errors.As(got, &pe) {
		t.Fatalf("expected *common.ErrPaymentRequired, got %T", got)
	}
	resp := pe.PaymentRequirements.(X402PaymentRequirementsResponse)
	if resp.Error != first.Error {
		t.Errorf("Error: want %q (from first), got %q", first.Error, resp.Error)
	}
	if got, want := resp.Resource.(map[string]string)["url"], "https://edge.test/standard/evm/1"; got != want {
		t.Errorf("Resource.url: want %q (from first), got %q", want, got)
	}
}

// Authenticate-level integration test: configure an AuthRegistry with two real
// x402 strategies (different chains) and verify an unauthenticated request
// gets back ONE merged ErrPaymentRequired containing both networks in Accepts.
func TestAuthRegistry_Authenticate_MergesMultiX402_402(t *testing.T) {
	logger := zerolog.Nop()
	rlReg, err := upstream.NewRateLimitersRegistry(context.Background(), nil, &logger)
	if err != nil {
		t.Fatalf("NewRateLimitersRegistry: %v", err)
	}

	// Stub facilitator returns /supported with both chains advertised so each
	// strategy initializes successfully (it consults /supported at construction).
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/supported" {
			_ = json.NewEncoder(w).Encode(X402SupportedResponse{
				Kinds: []X402SupportedKind{
					{X402Version: 2, Scheme: "exact", Network: "eip155:8453"},
					{X402Version: 2, Scheme: "exact", Network: "eip155:1"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer facilitator.Close()

	mkStrategy := func(network, asset string) common.AuthStrategyConfig {
		return common.AuthStrategyConfig{
			Type: common.AuthTypeX402,
			X402: &common.X402StrategyConfig{
				FacilitatorURL:    facilitator.URL,
				SellerAddress:     "0xSeller",
				PricePerRequest:   "5",
				Network:           network,
				Asset:             asset,
				Scheme:            "exact",
				MaxTimeoutSeconds: 604800,
			},
		}
	}
	cfg := &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			pointer(mkStrategy("eip155:8453", "0xUSDC-base")),
			pointer(mkStrategy("eip155:1", "0xUSDC-eth")),
		},
	}
	registry, err := NewAuthRegistry(context.Background(), &logger, "test-project", cfg, rlReg)
	if err != nil {
		t.Fatalf("NewAuthRegistry: %v", err)
	}

	ap := &AuthPayload{Type: common.AuthTypeNetwork, Method: "eth_chainId"}
	_, err = registry.Authenticate(context.Background(), nil, "eth_chainId", ap)
	if err == nil {
		t.Fatal("expected ErrPaymentRequired, got nil")
	}

	var pe *common.ErrPaymentRequired
	if !errors.As(err, &pe) {
		t.Fatalf("expected *common.ErrPaymentRequired, got %T: %v", err, err)
	}
	resp, ok := pe.PaymentRequirements.(X402PaymentRequirementsResponse)
	if !ok {
		t.Fatalf("expected merged X402PaymentRequirementsResponse, got %T", pe.PaymentRequirements)
	}
	if len(resp.Accepts) != 2 {
		t.Fatalf("Accepts: want 2 networks (merged), got %d: %+v", len(resp.Accepts), resp.Accepts)
	}
	if resp.Accepts[0].Network != "eip155:8453" || resp.Accepts[1].Network != "eip155:1" {
		t.Errorf("Accepts order: want [eip155:8453, eip155:1], got [%s, %s]",
			resp.Accepts[0].Network, resp.Accepts[1].Network)
	}
}

func pointer[T any](v T) *T { return &v }

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
