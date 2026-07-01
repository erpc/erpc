package common

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// SkipCacheWrite directive
// ----------------------------------------------------------------------------

func TestSkipCacheWriteDirective_DefaultIsFalse(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	req.EnrichFromHttp(http.Header{}, url.Values{}, UserAgentTrackingModeSimplified)

	if req.ShouldSkipCacheWrite() {
		t.Fatalf("expected ShouldSkipCacheWrite=false by default, got true")
	}
}

func TestSkipCacheWriteDirective_FromHeader(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"TRUE", true},
		{"  true  ", true},
		{"false", false},
		{"1", false}, // only "true" is truthy, mirroring the other bool directives
		{"yes", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("X-ERPC-Skip-Cache-Write=%s", tc.value), func(t *testing.T) {
			req := NewNormalizedRequest(nil)
			h := http.Header{}
			h.Set("X-ERPC-Skip-Cache-Write", tc.value)
			req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
			if got := req.ShouldSkipCacheWrite(); got != tc.want {
				t.Fatalf("expected ShouldSkipCacheWrite=%v, got %v", tc.want, got)
			}
		})
	}
}

func TestSkipCacheWriteDirective_FromQuery(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"TRUE", true},
		{"false", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("skip-cache-write=%q", tc.value), func(t *testing.T) {
			req := NewNormalizedRequest(nil)
			q := url.Values{}
			if tc.value != "" {
				q.Set("skip-cache-write", tc.value)
			}
			req.EnrichFromHttp(nil, q, UserAgentTrackingModeSimplified)
			if got := req.ShouldSkipCacheWrite(); got != tc.want {
				t.Fatalf("expected ShouldSkipCacheWrite=%v, got %v", tc.want, got)
			}
		})
	}
}

func TestSkipCacheWriteDirective_DefaultsFromConfig(t *testing.T) {
	tr := true
	fa := false

	t.Run("default_true_applies_when_no_request_override", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheWrite: &tr})
		if !req.ShouldSkipCacheWrite() {
			t.Fatalf("expected SkipCacheWrite=true from defaults")
		}
	})

	t.Run("default_false_applies_when_no_request_override", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheWrite: &fa})
		if req.ShouldSkipCacheWrite() {
			t.Fatalf("expected SkipCacheWrite=false from defaults")
		}
	})

	t.Run("nil_default_leaves_false", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{})
		if req.ShouldSkipCacheWrite() {
			t.Fatalf("expected SkipCacheWrite=false when default unset")
		}
	})

	t.Run("header_overrides_default_true_to_false", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheWrite: &tr})
		h := http.Header{}
		h.Set("X-ERPC-Skip-Cache-Write", "false")
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		if req.ShouldSkipCacheWrite() {
			t.Fatalf("expected SkipCacheWrite=false after header override")
		}
	})

	t.Run("query_overrides_default_false_to_true", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheWrite: &fa})
		q := url.Values{}
		q.Set("skip-cache-write", "true")
		req.EnrichFromHttp(nil, q, UserAgentTrackingModeSimplified)
		if !req.ShouldSkipCacheWrite() {
			t.Fatalf("expected SkipCacheWrite=true after query override")
		}
	})
}

func TestSkipCacheWriteDirective_Clone(t *testing.T) {
	d := &RequestDirectives{SkipCacheWrite: true}
	cloned := d.Clone()
	if !cloned.SkipCacheWrite {
		t.Fatalf("Clone() did not preserve SkipCacheWrite")
	}
	// Independent copy: mutating the clone must not affect the original.
	cloned.SkipCacheWrite = false
	if !d.SkipCacheWrite {
		t.Fatalf("Clone() returned an aliased reference; original mutated")
	}
}

func TestShouldSkipCacheWrite_NilSafety(t *testing.T) {
	var nilReq *NormalizedRequest
	if nilReq.ShouldSkipCacheWrite() {
		t.Fatalf("nil request must report false")
	}
	req := NewNormalizedRequest(nil) // directives not yet populated
	if req.ShouldSkipCacheWrite() {
		t.Fatalf("request with no directives must report false")
	}
}
