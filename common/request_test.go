package common

import (
	"context"
	"fmt"
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
func (m *mockUpstreamForSelection) Forward(ctx context.Context, nq *NormalizedRequest, byPass, isHedgeAttempt bool) (*NormalizedResponse, error) {
	return nil, nil
}
func (m *mockUpstreamForSelection) Cordon(method string, reason string)   {}
func (m *mockUpstreamForSelection) Uncordon(method string, reason string) {}
func (m *mockUpstreamForSelection) IgnoreMethod(method string)            {}
func (m *mockUpstreamForSelection) ShouldHandleMethod(method string) (bool, error) {
	return true, nil
}

func newMockUpstream(id string) *mockUpstreamForSelection {
	return &mockUpstreamForSelection{id: id}
}

// TestUpstreamSelection_NonRetryableError_Skipped tests that non-retryable permanent
// errors (like method not supported) cause the upstream to be gated on subsequent
// selections. The ErrorsByUpstream gate blocks non-retryable, non-MissingData errors.
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

	// Verify upstream is gated (ErrorsByUpstream gate blocks non-retryable,
	// non-MissingData errors).
	_, err = req.NextUpstream()
	if !HasErrorCode(err, ErrCodeNoUpstreamsLeftToSelect) {
		t.Fatalf("expected no upstreams left after non-retryable permanent error, got: %v", err)
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

// TestUpstreamSelection_EmptyResponses_DontBlockReselection tests that upstreams
// which returned empty results can be re-selected on a subsequent retry round.
// BUG (before fix): EmptyResponses gate in NextUpstream permanently blocks
// upstreams that returned empty, preventing useful retries.
func TestUpstreamSelection_EmptyResponses_DontBlockReselection(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("rpc1")
	up2 := newMockUpstream("rpc2")
	req.SetUpstreams([]Upstream{up1, up2})

	// Round 1: select both upstreams and mark them as returning empty
	selected1, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("first NextUpstream should succeed: %v", err)
	}
	emptyResp1 := createEmptyNormalizedResponse(t)
	req.MarkUpstreamCompleted(ctx, selected1, emptyResp1, nil)

	selected2, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("second NextUpstream should succeed: %v", err)
	}
	emptyResp2 := createEmptyNormalizedResponse(t)
	req.MarkUpstreamCompleted(ctx, selected2, emptyResp2, nil)

	// Simulate "next retry round": both upstreams should be re-selectable.
	// BUG (before fix): EmptyResponses gate permanently blocks both upstreams
	// → NextUpstream returns ErrNoUpstreamsLeftToSelect.
	reselected, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("upstreams that returned empty should be re-selectable for retry, but got: %v", err)
	}
	if reselected == nil {
		t.Fatalf("expected a valid upstream to be returned")
	}
	t.Logf("re-selected upstream: %s", reselected.Id())
}

// TestUpstreamSelection_MissingDataError_DontBlockReselection tests that upstreams
// which returned MissingData errors can be re-selected on a subsequent retry round.
// BUG (before fix): ErrorsByUpstream gate treats ErrEndpointMissingData as
// non-retryable and permanently blocks the upstream.
func TestUpstreamSelection_MissingDataError_DontBlockReselection(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	up1 := newMockUpstream("rpc1")
	up2 := newMockUpstream("rpc2")
	req.SetUpstreams([]Upstream{up1, up2})

	// Round 1: select both upstreams and mark them with MissingData errors
	selected1, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("first NextUpstream should succeed: %v", err)
	}
	missingErr1 := NewErrEndpointMissingData(fmt.Errorf("missing trie node"), up1)
	req.MarkUpstreamCompleted(ctx, selected1, nil, missingErr1)

	selected2, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("second NextUpstream should succeed: %v", err)
	}
	missingErr2 := NewErrEndpointMissingData(fmt.Errorf("missing trie node"), up2)
	req.MarkUpstreamCompleted(ctx, selected2, nil, missingErr2)

	// Simulate "next retry round": both upstreams should be re-selectable.
	// BUG (before fix): ErrorsByUpstream gate sees ErrEndpointMissingData as
	// non-retryable toward upstream → permanently blocks both.
	reselected, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("upstreams with MissingData errors should be re-selectable for retry, but got: %v", err)
	}
	if reselected == nil {
		t.Fatalf("expected a valid upstream to be returned")
	}
	t.Logf("re-selected upstream: %s", reselected.Id())
}

func createEmptyNormalizedResponse(t *testing.T) *NormalizedResponse {
	t.Helper()
	jrr, err := NewJsonRpcResponse(1, nil, nil)
	if err != nil {
		t.Fatalf("failed to create empty JSON-RPC response: %v", err)
	}
	return NewNormalizedResponse().WithJsonRpcResponse(jrr)
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

// TestHeaderOverridesConfigDefault_ValidateTransactionsRoot verifies that when the
// config defaults set ValidateTransactionsRoot=true, a header/query-string can
// override it to false.
func TestHeaderOverridesConfigDefault_ValidateTransactionsRoot(t *testing.T) {
	trueVal := true
	cfgDefaults := &DirectiveDefaultsConfig{
		ValidateTransactionsRoot: &trueVal,
	}

	t.Run("header_overrides_config_true_to_false", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))
		req.ApplyDirectiveDefaults(cfgDefaults)

		if dir := req.Directives(); dir == nil || !dir.ValidateTransactionsRoot {
			t.Fatalf("expected ValidateTransactionsRoot=true after ApplyDirectiveDefaults")
		}

		headers := http.Header{}
		headers.Set("X-ERPC-Validate-Transactions-Root", "false")
		req.EnrichFromHttp(headers, nil, UserAgentTrackingModeSimplified)

		if dir := req.Directives(); dir == nil || dir.ValidateTransactionsRoot {
			t.Fatalf("expected ValidateTransactionsRoot=false after header override, but got true")
		}
	})

	t.Run("query_string_overrides_config_true_to_false", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))
		req.ApplyDirectiveDefaults(cfgDefaults)

		query := url.Values{}
		query.Set("validate-transactions-root", "false")
		req.EnrichFromHttp(nil, query, UserAgentTrackingModeSimplified)

		if dir := req.Directives(); dir == nil || dir.ValidateTransactionsRoot {
			t.Fatalf("expected ValidateTransactionsRoot=false after query string override, but got true")
		}
	})

	t.Run("header_and_query_both_false_override_config_true", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))
		req.ApplyDirectiveDefaults(cfgDefaults)

		headers := http.Header{}
		headers.Set("X-ERPC-Validate-Transactions-Root", "false")
		headers.Set("X-ERPC-Skip-Cache-Read", "true")

		query := url.Values{}
		query.Set("validate-transactions-root", "false")

		req.EnrichFromHttp(headers, query, UserAgentTrackingModeSimplified)

		dir := req.Directives()
		if dir == nil || dir.ValidateTransactionsRoot {
			t.Fatalf("expected ValidateTransactionsRoot=false after header+query override, but got true")
		}
		if dir.SkipCacheRead != "true" {
			t.Fatalf("expected SkipCacheRead='true' from header, got '%s'", dir.SkipCacheRead)
		}
	})
}

// ----------------------------------------------------------------------------
// SkipConsensus directive
// ----------------------------------------------------------------------------

func TestSkipConsensusDirective_DefaultIsFalse(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	req.EnrichFromHttp(http.Header{}, url.Values{}, UserAgentTrackingModeSimplified)

	if dir := req.Directives(); dir != nil && dir.SkipConsensus {
		t.Fatalf("expected SkipConsensus=false by default, got true")
	}
}

func TestSkipConsensusDirective_FromHeader(t *testing.T) {
	cases := []struct {
		header string
		value  string
		want   bool
	}{
		{"X-ERPC-Skip-Consensus", "true", true},
		{"X-ERPC-Skip-Consensus", "TRUE", true},
		{"X-ERPC-Skip-Consensus", "  true  ", true},
		{"X-ERPC-Skip-Consensus", "false", false},
		{"X-ERPC-Skip-Consensus", "1", false}, // only "true" is truthy
		{"X-ERPC-Skip-Consensus", "yes", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s=%s", tc.header, tc.value), func(t *testing.T) {
			req := NewNormalizedRequest(nil)
			h := http.Header{}
			h.Set(tc.header, tc.value)
			req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
			dir := req.Directives()
			if dir == nil {
				t.Fatalf("expected directives to be initialized")
			}
			if dir.SkipConsensus != tc.want {
				t.Fatalf("expected SkipConsensus=%v, got %v", tc.want, dir.SkipConsensus)
			}
		})
	}
}

func TestSkipConsensusDirective_FromQuery(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"true", true},
		{"TRUE", true},
		{" true ", true},
		{"false", false},
		{"", false}, // empty doesn't change default
		{"yes", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("?skip-consensus=%q", tc.query), func(t *testing.T) {
			req := NewNormalizedRequest(nil)
			q := url.Values{}
			if tc.query != "" {
				q.Set("skip-consensus", tc.query)
			}
			req.EnrichFromHttp(nil, q, UserAgentTrackingModeSimplified)
			dir := req.Directives()
			if dir == nil {
				if tc.want {
					t.Fatalf("expected SkipConsensus=%v but directives are nil", tc.want)
				}
				return
			}
			if dir.SkipConsensus != tc.want {
				t.Fatalf("expected SkipConsensus=%v, got %v", tc.want, dir.SkipConsensus)
			}
		})
	}
}

func TestSkipConsensusDirective_QueryOverridesHeader(t *testing.T) {
	// Documented precedence: query parameters apply after headers in the
	// parser, so an explicit query value wins.
	req := NewNormalizedRequest(nil)
	h := http.Header{}
	h.Set("X-ERPC-Skip-Consensus", "true")
	q := url.Values{}
	q.Set("skip-consensus", "false")
	req.EnrichFromHttp(h, q, UserAgentTrackingModeSimplified)

	if dir := req.Directives(); dir == nil || dir.SkipConsensus {
		t.Fatalf("expected SkipConsensus=false (query override), got %+v", dir)
	}
}

func TestSkipConsensusDirective_DefaultsFromConfig(t *testing.T) {
	tr := true
	fa := false

	t.Run("default_true_applies_when_no_request_override", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipConsensus: &tr})
		if dir := req.Directives(); dir == nil || !dir.SkipConsensus {
			t.Fatalf("expected SkipConsensus=true from defaults")
		}
	})

	t.Run("default_false_applies_when_no_request_override", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipConsensus: &fa})
		if dir := req.Directives(); dir == nil || dir.SkipConsensus {
			t.Fatalf("expected SkipConsensus=false from defaults")
		}
	})

	t.Run("header_overrides_default_true_to_false", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipConsensus: &tr})

		h := http.Header{}
		h.Set("X-ERPC-Skip-Consensus", "false")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)

		if dir := req.Directives(); dir == nil || dir.SkipConsensus {
			t.Fatalf("expected SkipConsensus=false after header override, got %+v", dir)
		}
	})

	t.Run("query_overrides_default_false_to_true", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipConsensus: &fa})

		q := url.Values{}
		q.Set("skip-consensus", "true")
		req.EnrichFromHttp(nil, q, UserAgentTrackingModeSimplified)

		if dir := req.Directives(); dir == nil || !dir.SkipConsensus {
			t.Fatalf("expected SkipConsensus=true after query override, got %+v", dir)
		}
	})
}

func TestSkipConsensusDirective_ClonePreservesValue(t *testing.T) {
	for _, v := range []bool{true, false} {
		t.Run(fmt.Sprintf("SkipConsensus=%v", v), func(t *testing.T) {
			d := &RequestDirectives{SkipConsensus: v}
			cloned := d.Clone()
			if cloned.SkipConsensus != v {
				t.Fatalf("Clone() did not preserve SkipConsensus: got %v, want %v", cloned.SkipConsensus, v)
			}
			// Mutating the clone must not affect the original.
			cloned.SkipConsensus = !v
			if d.SkipConsensus != v {
				t.Fatalf("Clone() returned an aliased reference; original mutated")
			}
		})
	}
}

func TestDirectiveAllowFilter(t *testing.T) {
	ptr := func(s string) *string { return &s }
	tests := []struct {
		name    string
		pattern *string
		key     string
		want    bool
	}{
		{"nil allows all", nil, "skip-cache-read", true},
		{"nil allows any key", nil, "use-upstream", true},
		{"empty string blocks all", ptr(""), "skip-cache-read", false},
		{"empty string blocks any key", ptr(""), "retry-empty", false},
		{"wildcard allows all", ptr("*"), "skip-cache-read", true},
		{"wildcard allows any key", ptr("*"), "use-upstream", true},
		{"exact match allows", ptr("skip-cache-read"), "skip-cache-read", true},
		{"exact match denies other", ptr("skip-cache-read"), "use-upstream", false},
		{"OR allows first", ptr("skip-cache-read | use-upstream"), "skip-cache-read", true},
		{"OR allows second", ptr("skip-cache-read | use-upstream"), "use-upstream", true},
		{"OR denies other", ptr("skip-cache-read | use-upstream"), "retry-empty", false},
		{"negation denies target", ptr("!skip-cache-read"), "skip-cache-read", false},
		{"negation allows other", ptr("!skip-cache-read"), "use-upstream", true},
		{"AND negation denies first", ptr("!skip-cache-read & !use-upstream"), "skip-cache-read", false},
		{"AND negation denies second", ptr("!skip-cache-read & !use-upstream"), "use-upstream", false},
		{"AND negation allows other", ptr("!skip-cache-read & !use-upstream"), "retry-empty", true},
		{"glob allows matching", ptr("retry-*"), "retry-empty", true},
		{"glob allows matching 2", ptr("retry-*"), "retry-pending", true},
		{"glob denies non-matching", ptr("retry-*"), "skip-cache-read", false},
		{"glob OR allows glob match", ptr("retry-* | skip-consensus"), "retry-empty", true},
		{"glob OR allows exact match", ptr("retry-* | skip-consensus"), "skip-consensus", true},
		{"glob OR denies other", ptr("retry-* | skip-consensus"), "use-upstream", false},
		{"grouped negation denies first", ptr("!(skip-cache-read | use-upstream)"), "skip-cache-read", false},
		{"grouped negation denies second", ptr("!(skip-cache-read | use-upstream)"), "use-upstream", false},
		{"grouped negation allows other", ptr("!(skip-cache-read | use-upstream)"), "retry-empty", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := NewNormalizedRequest(nil)
			if tc.pattern != nil {
				if *tc.pattern == "" {
					req.SetAllowClientDirectiveMatcher(DenyAllClientDirectives)
				} else {
					matcher, err := NewWildcardMatcher(*tc.pattern)
					if err != nil {
						t.Fatalf("failed to compile pattern %q: %v", *tc.pattern, err)
					}
					req.SetAllowClientDirectiveMatcher(matcher)
				}
			}
			got := req.isDirectiveAllowed(tc.key)
			if got != tc.want {
				patternStr := "<nil>"
				if tc.pattern != nil {
					patternStr = *tc.pattern
				}
				t.Fatalf("isDirectiveAllowed(%q, %q) = %v, want %v", tc.key, patternStr, got, tc.want)
			}
		})
	}
}

func setDirectiveFilter(t *testing.T, req *NormalizedRequest, pattern string) {
	t.Helper()
	if pattern == "" {
		req.SetAllowClientDirectiveMatcher(DenyAllClientDirectives)
		return
	}
	matcher, err := NewWildcardMatcher(pattern)
	if err != nil {
		t.Fatalf("failed to compile pattern %q: %v", pattern, err)
	}
	req.SetAllowClientDirectiveMatcher(matcher)
}

func TestEnrichFromHttp_AllowClientDirectives(t *testing.T) {
	t.Run("nil allows all directives", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		h := http.Header{}
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		h.Set("X-ERPC-Use-Upstream", "alchemy")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		dir := req.Directives()
		if dir.SkipCacheRead != "true" {
			t.Fatalf("expected SkipCacheRead=true, got %q", dir.SkipCacheRead)
		}
		if dir.UseUpstream != "alchemy" {
			t.Fatalf("expected UseUpstream=alchemy, got %q", dir.UseUpstream)
		}
	})

	t.Run("empty string blocks all directives", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		setDirectiveFilter(t, req, "")
		h := http.Header{}
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		h.Set("X-ERPC-Use-Upstream", "alchemy")
		h.Set("X-ERPC-Skip-Consensus", "true")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		dir := req.Directives()
		if dir == nil {
			t.Fatal("expected directives struct to exist")
		}
		if dir.SkipCacheRead != "" {
			t.Fatalf("expected SkipCacheRead blocked, got %q", dir.SkipCacheRead)
		}
		if dir.UseUpstream != "" {
			t.Fatalf("expected UseUpstream blocked, got %q", dir.UseUpstream)
		}
		if dir.SkipConsensus {
			t.Fatal("expected SkipConsensus=false")
		}
	})

	t.Run("negation blocks specific directive", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		setDirectiveFilter(t, req, "!skip-cache-read")
		h := http.Header{}
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		h.Set("X-ERPC-Use-Upstream", "alchemy")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		dir := req.Directives()
		if dir.SkipCacheRead != "" {
			t.Fatalf("expected SkipCacheRead blocked, got %q", dir.SkipCacheRead)
		}
		if dir.UseUpstream != "alchemy" {
			t.Fatalf("expected UseUpstream=alchemy (allowed), got %q", dir.UseUpstream)
		}
	})

	t.Run("blocks query params too", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		setDirectiveFilter(t, req, "!skip-cache-read")
		q := url.Values{}
		q.Set("skip-cache-read", "true")
		q.Set("use-upstream", "alchemy")
		req.EnrichFromHttp(nil, q, UserAgentTrackingModeSimplified)
		dir := req.Directives()
		if dir.SkipCacheRead != "" {
			t.Fatalf("expected SkipCacheRead blocked via query, got %q", dir.SkipCacheRead)
		}
		if dir.UseUpstream != "alchemy" {
			t.Fatalf("expected UseUpstream=alchemy via query (allowed), got %q", dir.UseUpstream)
		}
	})

	t.Run("user agent always extracted regardless of filter", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		setDirectiveFilter(t, req, "")
		h := http.Header{}
		h.Set("User-Agent", "curl/7.68.0")
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		if req.AgentName() == "" || req.AgentName() == "unknown" {
			t.Fatalf("expected user-agent to be extracted even when all directives blocked, got %q", req.AgentName())
		}
		dir := req.Directives()
		if dir != nil && dir.SkipCacheRead != "" {
			t.Fatalf("expected SkipCacheRead blocked, got %q", dir.SkipCacheRead)
		}
	})

	t.Run("directive defaults still apply when client directives blocked", func(t *testing.T) {
		req := NewNormalizedRequest(nil)
		retryEmpty := true
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{
			RetryEmpty: &retryEmpty,
		})
		setDirectiveFilter(t, req, "")
		h := http.Header{}
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		dir := req.Directives()
		if dir == nil {
			t.Fatal("expected directives from defaults to survive, got nil")
		}
		if !dir.RetryEmpty {
			t.Fatal("expected RetryEmpty=true from defaults even when client directives blocked")
		}
		if dir.SkipCacheRead != "" {
			t.Fatalf("expected SkipCacheRead blocked, got %q", dir.SkipCacheRead)
		}
	})
}

func newReqForUser() *NormalizedRequest {
	return NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))
}

func TestSetUserFromTrustedHeader(t *testing.T) {
	t.Run("sets the user id when none was resolved", func(t *testing.T) {
		req := newReqForUser()
		req.SetUserFromTrustedHeader("proj_edge_ep1")
		if u := req.User(); u == nil || u.Id != "proj_edge_ep1" {
			t.Fatalf("expected user id %q, got %+v", "proj_edge_ep1", u)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		req := newReqForUser()
		req.SetUserFromTrustedHeader("  proj_edge_ep1\n")
		if u := req.User(); u == nil || u.Id != "proj_edge_ep1" {
			t.Fatalf("expected trimmed user id, got %+v", u)
		}
	})

	t.Run("is a no-op for an empty or whitespace-only value", func(t *testing.T) {
		for _, v := range []string{"", "   ", "\t\n"} {
			req := newReqForUser()
			req.SetUserFromTrustedHeader(v)
			if u := req.User(); u != nil {
				t.Fatalf("expected no user for value %q, got %+v", v, u)
			}
		}
	})

	t.Run("never derives a rate-limit budget", func(t *testing.T) {
		req := newReqForUser()
		req.SetUserFromTrustedHeader("proj_edge_ep1")
		if u := req.User(); u == nil || u.RateLimitBudget != "" {
			t.Fatalf("trusted-header user must carry no budget, got %+v", u)
		}
	})

	t.Run("auth wins — does not overwrite an already-resolved user", func(t *testing.T) {
		req := newReqForUser()
		req.SetUser(&User{Id: "auth-resolved", RateLimitBudget: "tier-1"})
		req.SetUserFromTrustedHeader("header-user")
		if u := req.User(); u == nil || u.Id != "auth-resolved" || u.RateLimitBudget != "tier-1" {
			t.Fatalf("auth-resolved user must be preserved, got %+v", u)
		}
	})
}
