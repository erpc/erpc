package consensus

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

func mustResult(t *testing.T, reqRaw string, resultRaw string) *execResult {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(reqRaw))
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(resultRaw), nil)
	if err != nil {
		t.Fatalf("build json-rpc response: %v", err)
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return &execResult{Result: resp, Index: 0}
}

// Two participants can return byte-identical responses that are NOT
// comparable — e.g. the same getSlot evaluated at different commitment
// levels. The match key must split them into separate voting groups.
func TestResultOrErrorToHash_MatchKeySplitsGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	keys := map[*common.NormalizedResponse]string{}
	matchKeyFn := func(_ context.Context, resp *common.NormalizedResponse) string {
		return keys[resp]
	}

	mkResult := func(key string) *execResult {
		r := mustResult(t,
			`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`,
			`{"context":{"slot":123},"value":123}`,
		)
		keys[r.Result] = key
		return r
	}

	cfg := &config{matchKeyFn: matchKeyFn}

	a := mkResult("finalized")
	b := mkResult("finalized")
	c := mkResult("processed")

	ha, err := resultOrErrorToHash(a, ctx, cfg)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := resultOrErrorToHash(b, ctx, cfg)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	hc, err := resultOrErrorToHash(c, ctx, cfg)
	if err != nil {
		t.Fatalf("hash c: %v", err)
	}

	if ha != hb {
		t.Fatalf("same match key must group: %q vs %q", ha, hb)
	}
	if ha == hc {
		t.Fatalf("different match keys must split groups, both got %q", ha)
	}
}

// An empty match key preserves plain canonical-hash grouping exactly.
func TestResultOrErrorToHash_EmptyMatchKeyIsNeutral(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := mustResult(t,
		`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`,
		`{"context":{"slot":123},"value":123}`,
	)

	plain, err := resultOrErrorToHash(r, ctx, &config{})
	if err != nil {
		t.Fatalf("plain hash: %v", err)
	}
	neutral, err := resultOrErrorToHash(r, ctx, &config{
		matchKeyFn: func(context.Context, *common.NormalizedResponse) string { return "" },
	})
	if err != nil {
		t.Fatalf("neutral hash: %v", err)
	}
	if plain != neutral {
		t.Fatalf("empty match key must not alter the hash: %q vs %q", plain, neutral)
	}
}

// The match key mixes into the ignore-fields path as well, so an operator
// configuring ignoreFields does not silently collapse cross-commitment
// groups again.
func TestResultOrErrorToHash_MatchKeyComposesWithIgnoreFields(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), common.RequestContextKey,
		common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`)))
	cfg := &config{
		ignoreFields: map[string][]string{"getSlot": {"context"}},
		matchKeyFn:   func(context.Context, *common.NormalizedResponse) string { return "processed" },
	}
	r := mustResult(t,
		`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`,
		`{"context":{"slot":123},"value":123}`,
	)
	h, err := resultOrErrorToHash(r, ctx, cfg)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h == "" || h[:6] != "match:" {
		t.Fatalf("expected match-key-prefixed hash, got %q", h)
	}
}
