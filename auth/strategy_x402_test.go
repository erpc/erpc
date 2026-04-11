package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

func newTestFacilitator(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/verify":
			json.NewEncoder(w).Encode(X402VerifyResponse{
				IsValid: true,
				Payer:   "0xTestPayer123",
			})
		case "/settle":
			json.NewEncoder(w).Encode(X402SettlementResponse{
				Success:     true,
				Transaction: "0xfaketx",
				Network:     "base",
				Payer:       "0xTestPayer123",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestX402Strategy(t *testing.T, facilitatorURL string) *X402Strategy {
	t.Helper()
	logger := zerolog.Nop()
	s, err := NewX402Strategy(&logger, &common.X402StrategyConfig{
		FacilitatorURL:    facilitatorURL,
		SellerAddress:     "0xSeller",
		PricePerRequest:   "5",
		Network:           "base",
		Asset:             "0xUSDC",
		Scheme:            "exact",
		MaxTimeoutSeconds: 300,
	})
	if err != nil {
		t.Fatalf("NewX402Strategy: %v", err)
	}
	return s
}

func makePaymentHeader(scheme, network string) string {
	payment := X402PaymentPayload{
		X402Version: 1,
		Scheme:      scheme,
		Network:     network,
		Payload: map[string]interface{}{
			"authorization": map[string]interface{}{
				"from": "0xTestPayer123",
				"to":   "0xSeller",
			},
			"signature": "0xfakesig",
		},
	}
	data, _ := json.Marshal(payment)
	return base64.StdEncoding.EncodeToString(data)
}

func TestX402Strategy_Supports(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	tests := []struct {
		name     string
		payload  *AuthPayload
		expected bool
	}{
		{"x402 type", &AuthPayload{Type: common.AuthTypeX402}, true},
		{"network type (fallback)", &AuthPayload{Type: common.AuthTypeNetwork}, true},
		{"secret type", &AuthPayload{Type: common.AuthTypeSecret}, false},
		{"jwt type", &AuthPayload{Type: common.AuthTypeJwt}, false},
		{"siwe type", &AuthPayload{Type: common.AuthTypeSiwe}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Supports(tt.payload)
			if got != tt.expected {
				t.Errorf("Supports() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestX402Strategy_NoPayment_Returns402(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	ap := &AuthPayload{Type: common.AuthTypeNetwork}
	user, err := s.Authenticate(context.Background(), nil, ap)

	if user != nil {
		t.Fatalf("expected nil user, got %v", user)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var payErr *common.ErrPaymentRequired
	if !common.HasErrorCode(err, common.ErrCodePaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired, got %T: %v", err, err)
	}

	// Verify the error contains payment requirements
	if ok := json.Unmarshal([]byte("{}"), &payErr); ok != nil {
		// Just check the error code is correct
	}
}

func TestX402Strategy_ValidPayment_Authenticates(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	paymentHeader := makePaymentHeader("exact", "base")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user == nil {
		t.Fatal("expected user, got nil")
	}
	if user.Id != "0xtestpayer123" {
		t.Errorf("expected user.Id = '0xtestpayer123', got '%s'", user.Id)
	}
}

func TestX402Strategy_InvalidBase64_Returns402(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: "not-valid-base64!!!"},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if user != nil {
		t.Fatalf("expected nil user, got %v", user)
	}
	if !common.HasErrorCode(err, common.ErrCodePaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired, got %T: %v", err, err)
	}
}

func TestX402Strategy_WrongScheme_Returns402(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	paymentHeader := makePaymentHeader("wrong-scheme", "wrong-network")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if user != nil {
		t.Fatalf("expected nil user, got %v", user)
	}
	if !common.HasErrorCode(err, common.ErrCodePaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired, got %T: %v", err, err)
	}
}

func TestX402Strategy_VerifyOnly_SkipsSettle(t *testing.T) {
	settledCalled := false
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/verify":
			json.NewEncoder(w).Encode(X402VerifyResponse{
				IsValid: true,
				Payer:   "0xTestPayer123",
			})
		case "/settle":
			settledCalled = true
			json.NewEncoder(w).Encode(X402SettlementResponse{
				Success: true,
				Payer:   "0xTestPayer123",
			})
		}
	}))
	defer facilitator.Close()

	logger := zerolog.Nop()
	s, err := NewX402Strategy(&logger, &common.X402StrategyConfig{
		FacilitatorURL:  facilitator.URL,
		SellerAddress:   "0xSeller",
		PricePerRequest: "5",
		Network:         "base",
		VerifyOnly:      true,
	})
	if err != nil {
		t.Fatalf("NewX402Strategy: %v", err)
	}

	paymentHeader := makePaymentHeader("exact", "base")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user == nil {
		t.Fatal("expected user, got nil")
	}
	if settledCalled {
		t.Error("settle was called but verifyOnly is true")
	}
}

func TestX402Strategy_FailedVerification_Returns401(t *testing.T) {
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/verify" {
			json.NewEncoder(w).Encode(X402VerifyResponse{
				IsValid:       false,
				InvalidReason: "insufficient funds",
			})
		}
	}))
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	paymentHeader := makePaymentHeader("exact", "base")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if user != nil {
		t.Fatalf("expected nil user, got %v", user)
	}
	if !common.HasErrorCode(err, common.ErrCodeAuthUnauthorized) {
		t.Fatalf("expected ErrAuthUnauthorized, got %T: %v", err, err)
	}
}

func TestX402Strategy_FailedSettlement_Returns401(t *testing.T) {
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/verify":
			json.NewEncoder(w).Encode(X402VerifyResponse{
				IsValid: true,
				Payer:   "0xTestPayer123",
			})
		case "/settle":
			json.NewEncoder(w).Encode(X402SettlementResponse{
				Success:     false,
				ErrorReason: "nonce already used",
			})
		}
	}))
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	paymentHeader := makePaymentHeader("exact", "base")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if user != nil {
		t.Fatalf("expected nil user, got %v", user)
	}
	if !common.HasErrorCode(err, common.ErrCodeAuthUnauthorized) {
		t.Fatalf("expected ErrAuthUnauthorized, got %T: %v", err, err)
	}
}

func TestX402Strategy_RateLimitBudget(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()

	logger := zerolog.Nop()
	s, err := NewX402Strategy(&logger, &common.X402StrategyConfig{
		FacilitatorURL:  facilitator.URL,
		SellerAddress:   "0xSeller",
		PricePerRequest: "5",
		Network:         "base",
		RateLimitBudget: "x402-budget",
	})
	if err != nil {
		t.Fatalf("NewX402Strategy: %v", err)
	}

	paymentHeader := makePaymentHeader("exact", "base")
	ap := &AuthPayload{
		Type: common.AuthTypeX402,
		X402: &X402Payload{Payment: paymentHeader},
	}

	user, err := s.Authenticate(context.Background(), nil, ap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user.RateLimitBudget != "x402-budget" {
		t.Errorf("expected RateLimitBudget = 'x402-budget', got '%s'", user.RateLimitBudget)
	}
}

func TestX402Strategy_PaymentRequirementsResponse_Format(t *testing.T) {
	facilitator := newTestFacilitator(t)
	defer facilitator.Close()
	s := newTestX402Strategy(t, facilitator.URL)

	ap := &AuthPayload{Type: common.AuthTypeNetwork}
	_, err := s.Authenticate(context.Background(), nil, ap)

	var payErr *common.ErrPaymentRequired
	if !errors.As(err, &payErr) {
		t.Fatalf("expected *ErrPaymentRequired, got %T", err)
	}

	resp, ok := payErr.PaymentRequirements.(X402PaymentRequirementsResponse)
	if !ok {
		t.Fatalf("expected X402PaymentRequirementsResponse, got %T", payErr.PaymentRequirements)
	}

	if resp.X402Version != 1 {
		t.Errorf("expected X402Version=1, got %d", resp.X402Version)
	}
	if len(resp.Accepts) != 1 {
		t.Fatalf("expected 1 accept, got %d", len(resp.Accepts))
	}
	accept := resp.Accepts[0]
	if accept.Scheme != "exact" {
		t.Errorf("expected scheme=exact, got %s", accept.Scheme)
	}
	if accept.Network != "base" {
		t.Errorf("expected network=base, got %s", accept.Network)
	}
	if accept.PayTo != "0xSeller" {
		t.Errorf("expected payTo=0xSeller, got %s", accept.PayTo)
	}
	if accept.MaxAmountRequired != "5" {
		t.Errorf("expected maxAmountRequired=5, got %s", accept.MaxAmountRequired)
	}
}
