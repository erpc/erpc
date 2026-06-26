package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// fakeFinalityNetwork is a minimal common.Network stub that lets a test
// pin a request's network id and finality without standing up a real
// network/state-poller. Only Id() and GetFinality() carry behavior; the
// rest return zero values.
type fakeFinalityNetwork struct {
	id       string
	finality common.DataFinalityState
}

func (n *fakeFinalityNetwork) Id() string        { return n.id }
func (n *fakeFinalityNetwork) Label() string     { return n.id }
func (n *fakeFinalityNetwork) ProjectId() string { return "test" }
func (n *fakeFinalityNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *fakeFinalityNetwork) Config() *common.NetworkConfig                 { return nil }
func (n *fakeFinalityNetwork) Logger() *zerolog.Logger                       { l := zerolog.Nop(); return &l }
func (n *fakeFinalityNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (n *fakeFinalityNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (n *fakeFinalityNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}
func (n *fakeFinalityNetwork) EvmHighestLatestBlockNumber(context.Context) int64    { return 0 }
func (n *fakeFinalityNetwork) EvmHighestFinalizedBlockNumber(context.Context) int64 { return 0 }
func (n *fakeFinalityNetwork) EvmLeaderUpstream(context.Context) common.Upstream    { return nil }

func newUpstreamReq(net *fakeFinalityNetwork, method string, params ...interface{}) *common.NormalizedRequest {
	r := common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{Method: method, Params: params})
	if net != nil {
		r.SetNetwork(net)
	}
	return r
}

func mustUpstreamExecutor(t *testing.T, cfg *common.UpstreamFailsafeConfig) *upstreamExecutor {
	t.Helper()
	lg := zerolog.Nop()
	ex, err := NewUpstreamExecutor(cfg, &lg)
	if err != nil {
		t.Fatalf("NewUpstreamExecutor: %v", err)
	}
	return ex
}

func TestUpstreamGetFailsafeExecutor_Matchers(t *testing.T) {
	ctx := context.Background()
	lg := zerolog.Nop()
	noop, _ := NewUpstreamExecutor(nil, &lg)

	t.Run("matcher selects by method, falls back to noop otherwise", func(t *testing.T) {
		logsExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_getLogs", Action: common.MatcherInclude}},
		})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{logsExec, noop}}

		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_getLogs")); got != logsExec {
			t.Fatalf("eth_getLogs: expected matcher executor, got %p", got)
		}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_call")); got != noop {
			t.Fatalf("eth_call: expected noop fallback, got %p", got)
		}
	})

	t.Run("matcher selects by params", func(t *testing.T) {
		latestExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_getBlockByNumber", Params: []interface{}{"latest"}, Action: common.MatcherInclude}},
		})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{latestExec, noop}}

		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_getBlockByNumber", "latest", false)); got != latestExec {
			t.Fatalf("latest: expected matcher executor, got %p", got)
		}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_getBlockByNumber", "0x1", false)); got != noop {
			t.Fatalf("0x1: expected noop fallback, got %p", got)
		}
	})

	t.Run("matcher selects by finality and network id", func(t *testing.T) {
		net := &fakeFinalityNetwork{id: "evm:1", finality: common.DataFinalityStateRealtime}
		rtExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{
			Matchers: []*common.MatcherConfig{{
				Network:  "evm:1",
				Finality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Action:   common.MatcherInclude,
			}},
		})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{rtExec, noop}}

		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(net, "eth_blockNumber")); got != rtExec {
			t.Fatalf("realtime/evm:1: expected matcher executor, got %p", got)
		}
		// Wrong network id -> no matcher match -> noop.
		otherNet := &fakeFinalityNetwork{id: "evm:137", finality: common.DataFinalityStateRealtime}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(otherNet, "eth_blockNumber")); got != noop {
			t.Fatalf("evm:137: expected noop fallback, got %p", got)
		}
		// Right network, wrong finality -> noop.
		finalNet := &fakeFinalityNetwork{id: "evm:1", finality: common.DataFinalityStateFinalized}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(finalNet, "eth_blockNumber")); got != noop {
			t.Fatalf("finalized: expected noop fallback, got %p", got)
		}
	})

	t.Run("exclude action opts the executor out", func(t *testing.T) {
		excludeExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_sendRawTransaction", Action: common.MatcherExclude}},
		})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{excludeExec, noop}}
		// Excluded method must not select excludeExec; falls to noop.
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_sendRawTransaction")); got != noop {
			t.Fatalf("excluded method: expected noop, got %p", got)
		}
	})

	t.Run("matcher executor takes precedence over a later legacy executor", func(t *testing.T) {
		matcherExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_call", Action: common.MatcherInclude}},
		})
		legacyExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{MatchMethod: "eth_call"})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{matcherExec, legacyExec, noop}}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_call")); got != matcherExec {
			t.Fatalf("expected matcher executor to win over legacy, got %p", got)
		}
	})

	t.Run("legacy executors keep working when no matchers configured", func(t *testing.T) {
		legacyExec := mustUpstreamExecutor(t, &common.UpstreamFailsafeConfig{MatchMethod: "eth_getLogs"})
		u := &Upstream{failsafeExecutors: []*upstreamExecutor{legacyExec, noop}}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_getLogs")); got != legacyExec {
			t.Fatalf("expected legacy executor for eth_getLogs, got %p", got)
		}
		if got := u.getFailsafeExecutor(ctx, newUpstreamReq(nil, "eth_call")); got != noop {
			t.Fatalf("expected noop for unmatched eth_call, got %p", got)
		}
	})
}
