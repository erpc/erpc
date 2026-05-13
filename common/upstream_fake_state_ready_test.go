package common

import (
	"context"
	"testing"
)

// TestFakeEvmStatePoller_StateReadyBlock_FallsBackToLatest verifies the
// backward-compat contract: a fake constructed without a state-readiness
// override returns LatestBlock from StateReadyBlock(), so tests using the
// "no probe configured" code path see expected behavior.
func TestFakeEvmStatePoller_StateReadyBlock_FallsBackToLatest(t *testing.T) {
	p := NewFakeEvmStatePoller(100, 50)
	if got := p.StateReadyBlock(); got != 100 {
		t.Fatalf("StateReadyBlock() = %d, want LatestBlock fallback = 100", got)
	}
}

// TestFakeEvmStatePoller_StateReadyBlock_RespectsOverride verifies the
// constructor that decouples StateReadyBlock from LatestBlock — used by
// tests that exercise head-vs-trie races.
func TestFakeEvmStatePoller_StateReadyBlock_RespectsOverride(t *testing.T) {
	p := NewFakeEvmStatePollerWithStateReady(100, 50, 95)
	if got := p.LatestBlock(); got != 100 {
		t.Fatalf("LatestBlock() = %d, want 100", got)
	}
	if got := p.FinalizedBlock(); got != 50 {
		t.Fatalf("FinalizedBlock() = %d, want 50", got)
	}
	if got := p.StateReadyBlock(); got != 95 {
		t.Fatalf("StateReadyBlock() = %d, want override 95", got)
	}
}

// TestFakeEvmStatePoller_PollStateReady documents the two behaviors of
// PollStateReady on the fake: zero-result when no probe is configured (so
// the upstream-side flow doesn't accidentally rely on a poll happening),
// and the override value when one is set.
func TestFakeEvmStatePoller_PollStateReady(t *testing.T) {
	t.Run("NoProbeConfigured_ReturnsZero", func(t *testing.T) {
		p := NewFakeEvmStatePoller(100, 50)
		got, err := p.PollStateReady(context.Background())
		if err != nil {
			t.Fatalf("PollStateReady() error: %v", err)
		}
		if got != 0 {
			t.Fatalf("PollStateReady() = %d, want 0 (no probe)", got)
		}
	})

	t.Run("ProbeConfigured_ReturnsOverride", func(t *testing.T) {
		p := NewFakeEvmStatePollerWithStateReady(100, 50, 95)
		got, err := p.PollStateReady(context.Background())
		if err != nil {
			t.Fatalf("PollStateReady() error: %v", err)
		}
		if got != 95 {
			t.Fatalf("PollStateReady() = %d, want 95", got)
		}
	})
}

// TestFakeEvmStatePoller_Diagnostics confirms the new diagnostic fields are
// populated correctly by the fake (used in /healthcheck integration tests).
func TestFakeEvmStatePoller_Diagnostics(t *testing.T) {
	t.Run("WithoutProbe_FallsBack", func(t *testing.T) {
		p := NewFakeEvmStatePoller(100, 50)
		d := p.GetDiagnostics()
		if d.StateProbeConfigured {
			t.Fatalf("StateProbeConfigured should be false without probe")
		}
		if d.StateReadyBlock != 100 {
			t.Fatalf("StateReadyBlock in diagnostics = %d, want 100 fallback", d.StateReadyBlock)
		}
	})

	t.Run("WithProbe_ReportsOverride", func(t *testing.T) {
		p := NewFakeEvmStatePollerWithStateReady(100, 50, 95)
		d := p.GetDiagnostics()
		if !d.StateProbeConfigured {
			t.Fatalf("StateProbeConfigured should be true with probe")
		}
		if d.StateReadyBlock != 95 {
			t.Fatalf("StateReadyBlock in diagnostics = %d, want 95", d.StateReadyBlock)
		}
	})
}
