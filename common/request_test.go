package common

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/rs/zerolog"
)

// mockUpstreamForSelection is a minimal mock for testing upstream selection logic
type mockUpstreamForSelection struct {
	id string
}

func (m *mockUpstreamForSelection) Id() string              { return m.id }
func (m *mockUpstreamForSelection) VendorName() string      { return "mock" }
func (m *mockUpstreamForSelection) NetworkId() string       { return "evm:1" }
func (m *mockUpstreamForSelection) NetworkLabel() string    { return "test" }
func (m *mockUpstreamForSelection) Config() *UpstreamConfig { return &UpstreamConfig{Id: m.id} }
func (m *mockUpstreamForSelection) Logger() *zerolog.Logger { return nil }
func (m *mockUpstreamForSelection) Vendor() Vendor          { return nil }
func (m *mockUpstreamForSelection) Tracker() HealthTracker  { return nil }
func (m *mockUpstreamForSelection) Forward(ctx context.Context, nq *NormalizedRequest, byPass bool) (*NormalizedResponse, error) {
	return nil, nil
}
func (m *mockUpstreamForSelection) Cordon(method string, reason string)   {}
func (m *mockUpstreamForSelection) Uncordon(method string, reason string) {}
func (m *mockUpstreamForSelection) IgnoreMethod(method string)            {}

func newMockUpstream(id string) *mockUpstreamForSelection {
	return &mockUpstreamForSelection{id: id}
}

// TestUpstreamSelection_NonRetryableError_Skipped tests that non-retryable errors
// cause the upstream to be skipped on subsequent selections.
func TestUpstreamSelection_NonRetryableError_Skipped(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("upstream1")
	req.SetUpstreams([]Upstream{up1})

	// First selection
	selected1, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("first NextUpstream should succeed: %v", err)
	}

	// Simulate a NON-retryable error (like method not supported)
	nonRetryableErr := NewErrUpstreamRequestSkipped(nil, "upstream1")
	req.MarkUpstreamCompleted(ctx, selected1, nil, nonRetryableErr)

	// Verify upstream is exhausted (stays in ConsumedUpstreams because canReUse=false for non-retryable)
	_, err = req.NextUpstream()
	if !HasErrorCode(err, ErrCodeNoUpstreamsLeftToSelect) {
		t.Fatalf("expected no upstreams left after non-retryable error")
	}
}

// TestUpstreamSelection_RetryableError_ClearedInSameCall tests that retryable errors
// are cleared and upstream is returned in the SAME call (no wasted attempts).
// This implements "try others first, then come back to retry" within a single NextUpstream call.
func TestUpstreamSelection_RetryableError_ClearedInSameCall(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("upstream1")
	req.SetUpstreams([]Upstream{up1})

	// First selection
	selected1, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("first NextUpstream should succeed: %v", err)
	}
	if selected1.Id() != "upstream1" {
		t.Fatalf("expected upstream1, got %s", selected1.Id())
	}

	// Simulate a retryable error - upstream stays consumed but error is stored
	retryableErr := NewErrUpstreamBlockUnavailable("upstream1", 1000, 500, 400)
	req.MarkUpstreamCompleted(ctx, up1, nil, retryableErr)

	// Second call: upstream1 is consumed with retryable error.
	// NextUpstream should clear it at midpoint and return it in the SAME call.
	selected2, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("second NextUpstream should succeed (cleared and returned in same call): %v", err)
	}
	if selected2.Id() != "upstream1" {
		t.Fatalf("expected upstream1 to be re-selected after clearing, got %s", selected2.Id())
	}
}

// TestUpstreamSelection_ErrorsAccumulate tests that errors from multiple upstreams
// are accumulated in ErrorsByUpstream.
func TestUpstreamSelection_ErrorsAccumulate(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("upstream1")
	up2 := newMockUpstream("upstream2")
	req.SetUpstreams([]Upstream{up1, up2})

	// Select and fail upstream1 with retryable error
	selected1, _ := req.NextUpstream()
	req.MarkUpstreamCompleted(ctx, selected1, nil, NewErrUpstreamBlockUnavailable("upstream1", 1000, 500, 400))

	// Select and fail upstream2 with non-retryable error
	selected2, _ := req.NextUpstream()
	req.MarkUpstreamCompleted(ctx, selected2, nil, NewErrUpstreamRequestSkipped(nil, "upstream2"))

	// Verify both errors are stored
	errorCount := 0
	req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
		errorCount++
		return true
	})
	if errorCount != 2 {
		t.Fatalf("expected 2 errors in ErrorsByUpstream, got %d", errorCount)
	}
}

// TestUpstreamSelection_ExhaustionShouldReturnUpstreamNotError tests that when
// NextUpstream exhausts and clears retryable errors, it should immediately return
// an available upstream instead of returning an error. This prevents "wasted"
// attempts where one call is sacrificed just to trigger the clearing.
//
// Current behavior (FLAWED): exhaustion returns error, next call gets the upstream
// Desired behavior: exhaustion clears and returns upstream in same call
func TestUpstreamSelection_ExhaustionShouldReturnUpstreamNotError(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("upstream1")
	up2 := newMockUpstream("upstream2")
	req.SetUpstreams([]Upstream{up1, up2})

	// Select both upstreams and mark them with retryable errors
	selected1, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("first NextUpstream should succeed: %v", err)
	}
	req.MarkUpstreamCompleted(ctx, selected1, nil, NewErrUpstreamBlockUnavailable("upstream1", 1000, 500, 400))

	selected2, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("second NextUpstream should succeed: %v", err)
	}
	req.MarkUpstreamCompleted(ctx, selected2, nil, NewErrUpstreamBlockUnavailable("upstream2", 1000, 500, 400))

	// Third call: both upstreams are consumed with retryable errors.
	// DESIRED: NextUpstream should clear retryables AND return an upstream in the same call.
	// CURRENT (FLAWED): NextUpstream returns error, "wasting" this attempt.
	selected3, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("third NextUpstream should return an upstream after clearing retryables, but got error: %v", err)
	}
	if selected3 == nil {
		t.Fatalf("third NextUpstream should return a valid upstream")
	}
	t.Logf("third call returned upstream: %s", selected3.Id())
}

// TestUpstreamSelection_MultipleExhaustionsNoWastedAttempts tests that with multiple
// upstreams and multiple rounds of exhaustion, no attempts are wasted.
// Each call to NextUpstream should either return an upstream or a final error.
func TestUpstreamSelection_MultipleExhaustionsNoWastedAttempts(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("upstream1")
	up2 := newMockUpstream("upstream2")
	up3 := newMockUpstream("upstream3")
	req.SetUpstreams([]Upstream{up1, up2, up3})

	// Simulate 6 consecutive calls (2 rounds of 3 upstreams)
	// Each upstream should be selectable twice without any "wasted" error-only calls
	selectedCount := 0
	for i := 0; i < 6; i++ {
		selected, err := req.NextUpstream()
		if err != nil {
			t.Fatalf("call %d: expected upstream but got error: %v", i+1, err)
		}
		selectedCount++
		t.Logf("call %d: selected %s", i+1, selected.Id())

		// Mark with retryable error
		req.MarkUpstreamCompleted(ctx, selected, nil, NewErrUpstreamBlockUnavailable(selected.Id(), 1000, 500, 400))
	}

	if selectedCount != 6 {
		t.Fatalf("expected 6 successful selections, got %d", selectedCount)
	}
}

func TestEnrichFromHttpHandlesBloomValidationHeaders(t *testing.T) {
	req := NewNormalizedRequest(nil)
	headers := http.Header{}
	headers.Set(headerDirectiveValidateLogsBloomEmpty, "true")

	req.EnrichFromHttp(headers, nil, UserAgentTrackingModeSimplified)

	dir := req.Directives()
	if dir == nil {
		t.Fatalf("expected directives to be initialized when headers are provided")
	}
	if !dir.ValidateLogsBloomEmptiness {
		t.Fatalf("expected ValidateLogsBloomEmptiness to be true")
	}
}

func TestEnrichFromHttpHandlesBloomValidationQueryParams(t *testing.T) {
	req := NewNormalizedRequest(nil)
	query := url.Values{}
	query.Set(queryDirectiveValidateLogsBloomMatch, "true")

	req.EnrichFromHttp(nil, query, UserAgentTrackingModeSimplified)

	dir := req.Directives()
	if dir == nil {
		t.Fatalf("expected directives to be initialized when query params are provided")
	}
	if !dir.ValidateLogsBloomMatch {
		t.Fatalf("expected ValidateLogsBloomMatch to be true")
	}
}
