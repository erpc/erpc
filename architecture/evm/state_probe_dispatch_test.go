package evm

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
)

// TestRunStateProbe_UnknownStrategy confirms the dispatch returns an error
// without touching the upstream when an unknown strategy slips past config
// validation (defense-in-depth — Validate() should catch this earlier, but
// the runtime must also refuse).
func TestRunStateProbe_UnknownStrategy(t *testing.T) {
	e := &EvmStatePoller{}
	probeCfg := &common.EvmStateProbeConfig{
		Strategy: "nonEmptyCode", // valid name but not implemented in v1
		Address:  "0xdead",
	}

	stored, ready, err := e.runStateProbe(context.Background(), probeCfg, 100, "prev")
	if err == nil {
		t.Fatalf("expected error for unknown strategy, got nil")
	}
	if !strings.Contains(err.Error(), "unknown strategy") {
		t.Fatalf("error %q does not mention unknown strategy", err.Error())
	}
	if ready {
		t.Fatalf("unknown strategy must not be ready")
	}
	if stored != "" {
		t.Fatalf("unknown strategy must not return a stored value")
	}
}

// TestRunStateProbe_EmptyStrategy confirms empty Strategy also routes to the
// default branch and errors (callers should be gated by Validate() but the
// runtime defends).
func TestRunStateProbe_EmptyStrategy(t *testing.T) {
	e := &EvmStatePoller{}
	probeCfg := &common.EvmStateProbeConfig{}

	_, ready, err := e.runStateProbe(context.Background(), probeCfg, 100, "")
	if err == nil {
		t.Fatalf("expected error for empty strategy")
	}
	if ready {
		t.Fatalf("empty strategy must not be ready")
	}
}
