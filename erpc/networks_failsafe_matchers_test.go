package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

func mustNetworkExecutor(t *testing.T, cfg *common.NetworkFailsafeConfig) *networkExecutor {
	t.Helper()
	lg := zerolog.Nop()
	ex, err := NewNetworkExecutor(cfg, &lg, nil, nil)
	if err != nil {
		t.Fatalf("NewNetworkExecutor: %v", err)
	}
	return ex
}

func networkReq(method string, params ...interface{}) *common.NormalizedRequest {
	return common.NewNormalizedRequestFromJsonRpcRequest(&common.JsonRpcRequest{Method: method, Params: params})
}

func TestNetworkGetFailsafeExecutor_Matchers(t *testing.T) {
	ctx := context.Background()
	lg := zerolog.Nop()
	noop, _ := NewNetworkExecutor(nil, &lg, nil, nil)

	t.Run("matcher selects by method, otherwise noop", func(t *testing.T) {
		logsExec := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_getLogs", Action: common.MatcherInclude}},
		})
		n := &Network{failsafeExecutors: []*networkExecutor{logsExec, noop}}

		if got := n.getFailsafeExecutor(ctx, networkReq("eth_getLogs")); got != logsExec {
			t.Fatalf("eth_getLogs: expected matcher executor, got %p", got)
		}
		if got := n.getFailsafeExecutor(ctx, networkReq("eth_call")); got != noop {
			t.Fatalf("eth_call: expected noop fallback, got %p", got)
		}
	})

	t.Run("matcher selects by params (positional prefix)", func(t *testing.T) {
		// Match eth_getLogs whose first param object has fromBlock "0x0".
		fullScanExec := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{
			Matchers: []*common.MatcherConfig{{
				Method: "eth_getLogs",
				Params: []interface{}{map[string]interface{}{"fromBlock": "0x0"}},
				Action: common.MatcherInclude,
			}},
		})
		n := &Network{failsafeExecutors: []*networkExecutor{fullScanExec, noop}}

		match := networkReq("eth_getLogs", map[string]interface{}{"fromBlock": "0x0", "toBlock": "latest"})
		if got := n.getFailsafeExecutor(ctx, match); got != fullScanExec {
			t.Fatalf("fromBlock 0x0: expected matcher executor, got %p", got)
		}
		noMatch := networkReq("eth_getLogs", map[string]interface{}{"fromBlock": "0x100", "toBlock": "latest"})
		if got := n.getFailsafeExecutor(ctx, noMatch); got != noop {
			t.Fatalf("fromBlock 0x100: expected noop fallback, got %p", got)
		}
	})

	t.Run("exclude action opts the executor out", func(t *testing.T) {
		exec := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_sendRawTransaction", Action: common.MatcherExclude}},
		})
		n := &Network{failsafeExecutors: []*networkExecutor{exec, noop}}
		if got := n.getFailsafeExecutor(ctx, networkReq("eth_sendRawTransaction")); got != noop {
			t.Fatalf("excluded method: expected noop, got %p", got)
		}
	})

	t.Run("matcher executor wins over a later legacy executor", func(t *testing.T) {
		matcherExec := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{
			Matchers: []*common.MatcherConfig{{Method: "eth_call", Action: common.MatcherInclude}},
		})
		legacyExec := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{MatchMethod: "eth_call"})
		n := &Network{failsafeExecutors: []*networkExecutor{matcherExec, legacyExec, noop}}
		if got := n.getFailsafeExecutor(ctx, networkReq("eth_call")); got != matcherExec {
			t.Fatalf("expected matcher executor to win, got %p", got)
		}
	})

	t.Run("legacy method matching unchanged when no matchers", func(t *testing.T) {
		// First-match-in-config-order is the network-scope legacy contract.
		first := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{MatchMethod: "eth_*"})
		second := mustNetworkExecutor(t, &common.NetworkFailsafeConfig{MatchMethod: "eth_getLogs"})
		n := &Network{failsafeExecutors: []*networkExecutor{first, second, noop}}
		// eth_getLogs matches the first (eth_*) by config order, not the more specific second.
		if got := n.getFailsafeExecutor(ctx, networkReq("eth_getLogs")); got != first {
			t.Fatalf("expected first-match (eth_*), got %p", got)
		}
		if got := n.getFailsafeExecutor(ctx, networkReq("net_version")); got != noop {
			t.Fatalf("expected noop for net_version, got %p", got)
		}
	})
}
