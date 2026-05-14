package common

import (
	"testing"
)

// stubUpstreamForKey is a minimal Upstream that only supports the call
// UniqueUpstreamKey actually makes (Config()). NetworkId is parameterised
// so we can prove the key does NOT depend on it.
type stubUpstreamForKey struct {
	cfg       *UpstreamConfig
	networkId string
	Upstream  // embed nil interface; we only use Config and NetworkId
}

func (s *stubUpstreamForKey) Config() *UpstreamConfig { return s.cfg }
func (s *stubUpstreamForKey) NetworkId() string       { return s.networkId }

func TestUniqueUpstreamKey_StableAcrossNetworkIdChanges(t *testing.T) {
	cfg := &UpstreamConfig{Id: "u1", Endpoint: "https://example/0"}

	// Before registration, NetworkId returns "n/a"; after registration it's
	// the real id. Both calls must produce the same key, otherwise the
	// per-upstream client cache and shared-state counters get duplicated
	// across the upstream's lifecycle.
	pre := UniqueUpstreamKey(&stubUpstreamForKey{cfg: cfg, networkId: "n/a"})
	post := UniqueUpstreamKey(&stubUpstreamForKey{cfg: cfg, networkId: "evm:1"})

	if pre != post {
		t.Fatalf("UniqueUpstreamKey changed when NetworkId changed: pre=%q post=%q", pre, post)
	}
}

func TestUniqueUpstreamKey_DeterministicAcrossHeaderOrder(t *testing.T) {
	mk := func(headers map[string]string) string {
		cfg := &UpstreamConfig{
			Id:       "u1",
			Endpoint: "https://example/0",
			JsonRpc:  &JsonRpcUpstreamConfig{Headers: headers},
		}
		return UniqueUpstreamKey(&stubUpstreamForKey{cfg: cfg, networkId: "evm:1"})
	}

	// Go map iteration is randomised, so the previous SHA-update order was
	// non-deterministic per call. Many runs against semantically identical
	// header maps must all produce the same key.
	headers := map[string]string{
		"Authorization": "Bearer xyz",
		"X-Tenant":      "tenant-a",
		"X-Region":      "eu-west-1",
		"X-Custom":      "value",
	}
	first := mk(headers)
	for i := range 50 {
		got := mk(headers)
		if got != first {
			t.Fatalf("UniqueUpstreamKey non-deterministic across map iterations: first=%q got=%q (iter %d)", first, got, i)
		}
	}
}

func TestUniqueUpstreamKey_DistinctConfigsProduceDistinctKeys(t *testing.T) {
	base := &UpstreamConfig{Id: "u1", Endpoint: "https://example/0"}
	keyBase := UniqueUpstreamKey(&stubUpstreamForKey{cfg: base, networkId: "evm:1"})

	cases := []struct {
		name string
		cfg  *UpstreamConfig
	}{
		{"different id", &UpstreamConfig{Id: "u2", Endpoint: "https://example/0"}},
		{"different endpoint", &UpstreamConfig{Id: "u1", Endpoint: "https://example/1"}},
		{"add headers", &UpstreamConfig{
			Id: "u1", Endpoint: "https://example/0",
			JsonRpc: &JsonRpcUpstreamConfig{Headers: map[string]string{"X": "1"}},
		}},
		{"different header value", &UpstreamConfig{
			Id: "u1", Endpoint: "https://example/0",
			JsonRpc: &JsonRpcUpstreamConfig{Headers: map[string]string{"X": "2"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UniqueUpstreamKey(&stubUpstreamForKey{cfg: tc.cfg, networkId: "evm:1"})
			if got == keyBase {
				t.Fatalf("expected distinct key, got same as base: %q", got)
			}
		})
	}
}
