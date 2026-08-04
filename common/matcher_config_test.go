package common

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// MatchConfig — single-matcher predicate (network/method/params/finality/empty)
// ---------------------------------------------------------------------------

func emptyPtr(b CacheEmptyBehavior) *CacheEmptyBehavior { return &b }

func TestMatchConfig(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *MatcherConfig
		networkId  string
		method     string
		params     []interface{}
		finality   DataFinalityState
		isEmptyish bool
		want       bool
	}{
		{
			name: "nil config never matches",
			cfg:  nil,
			want: false,
		},
		{
			name:   "empty matcher matches anything",
			cfg:    &MatcherConfig{},
			method: "eth_call", networkId: "evm:1",
			want: true,
		},
		{
			name:      "network exact match",
			cfg:       &MatcherConfig{Network: "evm:1"},
			networkId: "evm:1", method: "eth_call",
			want: true,
		},
		{
			name:      "network mismatch",
			cfg:       &MatcherConfig{Network: "evm:1"},
			networkId: "evm:137", method: "eth_call",
			want: false,
		},
		{
			name:      "network wildcard",
			cfg:       &MatcherConfig{Network: "evm:*"},
			networkId: "evm:42161",
			want:      true,
		},
		{
			name:   "method prefix wildcard",
			cfg:    &MatcherConfig{Method: "eth_*"},
			method: "eth_getLogs",
			want:   true,
		},
		{
			name:   "method mismatch",
			cfg:    &MatcherConfig{Method: "eth_call"},
			method: "eth_getLogs",
			want:   false,
		},
		{
			name:   "method boolean OR grammar",
			cfg:    &MatcherConfig{Method: "eth_call|eth_getLogs"},
			method: "eth_getLogs",
			want:   true,
		},
		{
			name:   "method negation grammar",
			cfg:    &MatcherConfig{Method: "!eth_sendRawTransaction"},
			method: "eth_call",
			want:   true,
		},
		{
			name:   "method negation grammar excludes",
			cfg:    &MatcherConfig{Method: "!eth_sendRawTransaction"},
			method: "eth_sendRawTransaction",
			want:   false,
		},
		{
			name:     "finality single member match",
			cfg:      &MatcherConfig{Finality: []DataFinalityState{DataFinalityStateFinalized}},
			finality: DataFinalityStateFinalized,
			want:     true,
		},
		{
			name:     "finality set OR match",
			cfg:      &MatcherConfig{Finality: []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnfinalized}},
			finality: DataFinalityStateUnfinalized,
			want:     true,
		},
		{
			name:     "finality set no member",
			cfg:      &MatcherConfig{Finality: []DataFinalityState{DataFinalityStateFinalized}},
			finality: DataFinalityStateRealtime,
			want:     false,
		},
		{
			name:   "params exact string match",
			cfg:    &MatcherConfig{Params: []interface{}{"latest"}},
			params: []interface{}{"latest"},
			want:   true,
		},
		{
			name:   "params numeric hex comparison >=",
			cfg:    &MatcherConfig{Params: []interface{}{">=0x100"}},
			params: []interface{}{"0x200"},
			want:   true,
		},
		{
			name:   "params numeric hex comparison fails",
			cfg:    &MatcherConfig{Params: []interface{}{">=0x100"}},
			params: []interface{}{"0x50"},
			want:   false,
		},
		{
			name:      "AND across fields all match",
			cfg:       &MatcherConfig{Network: "evm:1", Method: "eth_*", Finality: []DataFinalityState{DataFinalityStateRealtime}},
			networkId: "evm:1", method: "eth_blockNumber", finality: DataFinalityStateRealtime,
			want: true,
		},
		{
			name:      "AND across fields one fails",
			cfg:       &MatcherConfig{Network: "evm:1", Method: "eth_*", Finality: []DataFinalityState{DataFinalityStateRealtime}},
			networkId: "evm:1", method: "eth_blockNumber", finality: DataFinalityStateFinalized,
			want: false,
		},
		{
			name:       "empty=ignore filters emptyish",
			cfg:        &MatcherConfig{Empty: emptyPtr(CacheEmptyBehaviorIgnore)},
			isEmptyish: true,
			want:       false,
		},
		{
			name:       "empty=ignore keeps non-emptyish",
			cfg:        &MatcherConfig{Empty: emptyPtr(CacheEmptyBehaviorIgnore)},
			isEmptyish: false,
			want:       true,
		},
		{
			name:       "empty=only filters non-emptyish",
			cfg:        &MatcherConfig{Empty: emptyPtr(CacheEmptyBehaviorOnly)},
			isEmptyish: false,
			want:       false,
		},
		{
			name:       "empty=only keeps emptyish",
			cfg:        &MatcherConfig{Empty: emptyPtr(CacheEmptyBehaviorOnly)},
			isEmptyish: true,
			want:       true,
		},
		{
			name:       "nil empty pointer imposes no constraint (emptyish)",
			cfg:        &MatcherConfig{Method: "eth_getLogs"},
			method:     "eth_getLogs",
			isEmptyish: true,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchConfig(tt.cfg, tt.networkId, tt.method, tt.params, tt.finality, tt.isEmptyish)
			if got != tt.want {
				t.Fatalf("MatchConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MatchParams / MatchParam — recursive structural param matching
// ---------------------------------------------------------------------------

func TestMatchParams(t *testing.T) {
	tests := []struct {
		name    string
		pattern []interface{}
		params  []interface{}
		want    bool
	}{
		{"empty pattern matches anything", nil, []interface{}{"a", "b"}, true},
		{"prefix-only pattern leaves trailing unconstrained", []interface{}{"latest"}, []interface{}{"latest", map[string]interface{}{"x": 1.0}}, true},
		{"first element mismatch", []interface{}{"earliest"}, []interface{}{"latest"}, false},
		{"missing param matched against nil fails string pattern", []interface{}{"latest"}, []interface{}{}, false},
		{"two-element exact match", []interface{}{"0x1", "latest"}, []interface{}{"0x1", "latest"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchParams(tt.pattern, tt.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("MatchParams() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchParam(t *testing.T) {
	tests := []struct {
		name    string
		pattern interface{}
		param   interface{}
		want    bool
	}{
		{"string wildcard", "eth_*", "eth_call", true},
		{"string literal mismatch", "latest", "earliest", false},
		{"float equality via stringify", 1.0, 1.0, true},
		{"bool equality", true, true, true},
		{"object subset keys match", map[string]interface{}{"fromBlock": "0x1"}, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"}, true},
		{"object missing key fails", map[string]interface{}{"fromBlock": "0x1"}, map[string]interface{}{"toBlock": "0x2"}, false},
		{"object value mismatch fails", map[string]interface{}{"fromBlock": "0x9"}, map[string]interface{}{"fromBlock": "0x1"}, false},
		{"object pattern vs non-object param fails", map[string]interface{}{"a": "b"}, "scalar", false},
		{"array exact length match", []interface{}{"0x1", "0x2"}, []interface{}{"0x1", "0x2"}, true},
		{"array length mismatch fails", []interface{}{"0x1"}, []interface{}{"0x1", "0x2"}, false},
		{"array pattern vs non-array param fails", []interface{}{"0x1"}, "scalar", false},
		{"nested object inside array", []interface{}{map[string]interface{}{"k": "v*"}}, []interface{}{map[string]interface{}{"k": "value"}}, true},
		{"numeric comparison on hex param", ">=0x100000", "0x200000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchParam(tt.pattern, tt.param)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("MatchParam(%v, %v) = %v, want %v", tt.pattern, tt.param, got, tt.want)
			}
		})
	}
}

func TestParamToString(t *testing.T) {
	tests := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{1.0, "1"},
		{1.5, "1.5"},
		{true, "true"},
		{false, "false"},
		{int64(7), "7"},
	}
	for _, tt := range tests {
		if got := ParamToString(tt.in); got != tt.want {
			t.Fatalf("ParamToString(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ValidateMatchers — action defaulting + rejection
// ---------------------------------------------------------------------------

func TestValidateMatchers(t *testing.T) {
	t.Run("defaults empty action to include", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_call"}}
		if err := ValidateMatchers(ms); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ms[0].Action != MatcherInclude {
			t.Fatalf("action = %q, want include", ms[0].Action)
		}
	})
	t.Run("accepts explicit include/exclude", func(t *testing.T) {
		ms := []*MatcherConfig{{Action: MatcherInclude}, {Action: MatcherExclude}}
		if err := ValidateMatchers(ms); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("rejects invalid action", func(t *testing.T) {
		ms := []*MatcherConfig{{Action: "maybe"}}
		if err := ValidateMatchers(ms); err == nil {
			t.Fatal("expected error for invalid action")
		}
	})
	t.Run("rejects nil entry", func(t *testing.T) {
		ms := []*MatcherConfig{nil}
		if err := ValidateMatchers(ms); err == nil {
			t.Fatal("expected error for nil matcher")
		}
	})
	t.Run("rejects malformed method pattern", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_(call"}} // unbalanced paren
		if err := ValidateMatchers(ms); err == nil {
			t.Fatal("expected error for malformed method pattern")
		}
	})
	t.Run("rejects malformed network pattern", func(t *testing.T) {
		ms := []*MatcherConfig{{Network: "evm:(1"}}
		if err := ValidateMatchers(ms); err == nil {
			t.Fatal("expected error for malformed network pattern")
		}
	})
	t.Run("rejects malformed string param pattern", func(t *testing.T) {
		ms := []*MatcherConfig{{Params: []interface{}{"(0x1"}}}
		if err := ValidateMatchers(ms); err == nil {
			t.Fatal("expected error for malformed param pattern")
		}
	})
	t.Run("accepts valid patterns incl wildcards and numeric", func(t *testing.T) {
		ms := []*MatcherConfig{{Network: "evm:*", Method: "eth_*|net_version", Params: []interface{}{">=0x100", map[string]interface{}{"x": "y"}}}}
		if err := ValidateMatchers(ms); err != nil {
			t.Fatalf("unexpected error for valid patterns: %v", err)
		}
	})
}

// MatchMatchers must honor the Empty constraint when a real response is
// supplied (the cache-SET shape). Failsafe selection passes resp=nil, but
// the wiring from response -> isEmptyish must work for any future caller.
func TestMatchMatchers_EmptyConstraintWithResponse(t *testing.T) {
	ctx := context.Background()

	emptyResp := func() *NormalizedResponse {
		jrr, err := NewJsonRpcResponse(1, nil, nil) // null result => emptyish
		if err != nil {
			t.Fatalf("NewJsonRpcResponse: %v", err)
		}
		return NewNormalizedResponse().WithJsonRpcResponse(jrr)
	}
	nonEmptyResp := func() *NormalizedResponse {
		jrr, err := NewJsonRpcResponse(1, "0x1234", nil)
		if err != nil {
			t.Fatalf("NewJsonRpcResponse: %v", err)
		}
		return NewNormalizedResponse().WithJsonRpcResponse(jrr)
	}

	req := func() *NormalizedRequest {
		return NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{Method: "eth_getLogs"})
	}

	t.Run("empty=only includes emptyish response", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_getLogs", Empty: emptyPtr(CacheEmptyBehaviorOnly), Action: MatcherInclude}}
		if !MatchMatchers(ctx, ms, req(), emptyResp()) {
			t.Fatal("expected empty=only to include an emptyish response")
		}
		if MatchMatchers(ctx, ms, req(), nonEmptyResp()) {
			t.Fatal("expected empty=only to exclude a non-emptyish response")
		}
	})

	t.Run("empty=ignore excludes emptyish response", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_getLogs", Empty: emptyPtr(CacheEmptyBehaviorIgnore), Action: MatcherInclude}}
		if MatchMatchers(ctx, ms, req(), emptyResp()) {
			t.Fatal("expected empty=ignore to exclude an emptyish response")
		}
		if !MatchMatchers(ctx, ms, req(), nonEmptyResp()) {
			t.Fatal("expected empty=ignore to include a non-emptyish response")
		}
	})
}

// A matcher-based FailsafeConfig must survive SetDefaults and the
// network/upstream defaults-merge with its Matchers intact (the merge
// keys off matchMethod/matchFinality and only fills unset sub-policies;
// it must never drop Matchers). This locks in the invariant the selectors
// rely on.
func TestFailsafeConfig_MatchersSurviveDefaulting(t *testing.T) {
	t.Run("SetDefaults(nil) keeps Matchers", func(t *testing.T) {
		fs := &FailsafeConfig{
			Matchers: []*MatcherConfig{{Method: "eth_getLogs", Action: MatcherInclude}},
		}
		if err := fs.SetDefaults(nil); err != nil {
			t.Fatalf("SetDefaults: %v", err)
		}
		if len(fs.Matchers) != 1 || fs.Matchers[0].Method != "eth_getLogs" {
			t.Fatalf("Matchers dropped/mangled by SetDefaults: %+v", fs.Matchers)
		}
	})

	t.Run("SetDefaults(default) fills unset sub-policy but keeps Matchers", func(t *testing.T) {
		fs := &FailsafeConfig{
			Matchers: []*MatcherConfig{{Method: "eth_getLogs", Action: MatcherInclude}},
		}
		def := &FailsafeConfig{
			MatchMethod: "*",
			Retry:       &RetryPolicyConfig{MaxAttempts: 3, BackoffFactor: 1.2},
		}
		if err := fs.SetDefaults(def); err != nil {
			t.Fatalf("SetDefaults: %v", err)
		}
		if len(fs.Matchers) != 1 || fs.Matchers[0].Method != "eth_getLogs" {
			t.Fatalf("Matchers dropped by SetDefaults(default): %+v", fs.Matchers)
		}
		if fs.Retry == nil || fs.Retry.MaxAttempts != 3 {
			t.Fatalf("expected default retry to be inherited, got %+v", fs.Retry)
		}
	})

	t.Run("Validate allows empty matchMethod when matchers present", func(t *testing.T) {
		fs := &FailsafeConfig{
			Matchers: []*MatcherConfig{{Method: "eth_getLogs", Action: MatcherInclude}},
		}
		if err := fs.Validate(); err != nil {
			t.Fatalf("Validate rejected a matcher-only config: %v", err)
		}
	})

	t.Run("Validate still requires matchMethod when no matchers", func(t *testing.T) {
		fs := &FailsafeConfig{} // no matchers, empty matchMethod
		if err := fs.Validate(); err == nil {
			t.Fatal("expected Validate to require matchMethod when no matchers")
		}
	})
}

// ---------------------------------------------------------------------------
// Copy / CopyMatchers — deep independence
// ---------------------------------------------------------------------------

func TestMatcherConfigCopy(t *testing.T) {
	orig := &MatcherConfig{
		Network:  "evm:1",
		Method:   "eth_*",
		Params:   []interface{}{"latest"},
		Finality: []DataFinalityState{DataFinalityStateFinalized},
		Empty:    emptyPtr(CacheEmptyBehaviorOnly),
		Action:   MatcherInclude,
	}
	cp := orig.Copy()

	// Mutate the clone's slices/pointer; original must be unaffected.
	cp.Params[0] = "earliest"
	cp.Finality[0] = DataFinalityStateRealtime
	*cp.Empty = CacheEmptyBehaviorIgnore
	cp.Method = "net_*"

	if orig.Params[0] != "latest" {
		t.Fatalf("original Params mutated: %v", orig.Params)
	}
	if orig.Finality[0] != DataFinalityStateFinalized {
		t.Fatalf("original Finality mutated: %v", orig.Finality)
	}
	if *orig.Empty != CacheEmptyBehaviorOnly {
		t.Fatalf("original Empty mutated: %v", *orig.Empty)
	}
	if orig.Method != "eth_*" {
		t.Fatalf("original Method mutated: %v", orig.Method)
	}
	if (*MatcherConfig)(nil).Copy() != nil {
		t.Fatal("nil.Copy() should be nil")
	}
}

func TestCopyMatchers(t *testing.T) {
	if CopyMatchers(nil) != nil {
		t.Fatal("CopyMatchers(nil) should be nil")
	}
	orig := []*MatcherConfig{{Method: "eth_call", Params: []interface{}{"0x1"}}}
	cp := CopyMatchers(orig)
	cp[0].Params[0] = "0x2"
	if orig[0].Params[0] != "0x1" {
		t.Fatalf("original element mutated through copy: %v", orig[0].Params)
	}
}

// ---------------------------------------------------------------------------
// MatchMatchers — request-level include/exclude with last-wins precedence
// ---------------------------------------------------------------------------

func reqWith(method string, finality DataFinalityState, params ...interface{}) *NormalizedRequest {
	r := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{Method: method, Params: params})
	r.finality.Store(finality)
	return r
}

func TestMatchMatchers(t *testing.T) {
	ctx := context.Background()

	t.Run("nil request returns false", func(t *testing.T) {
		if MatchMatchers(ctx, []*MatcherConfig{{Method: "*", Action: MatcherInclude}}, nil, nil) {
			t.Fatal("expected false for nil request")
		}
	})

	t.Run("empty matcher slice returns false", func(t *testing.T) {
		if MatchMatchers(ctx, nil, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected false for empty matchers")
		}
	})

	t.Run("single include match", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_call", Action: MatcherInclude}}
		if !MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected include match")
		}
	})

	t.Run("no match returns false (excluded)", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_getLogs", Action: MatcherInclude}}
		if MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected no match -> false")
		}
	})

	t.Run("empty action treated as include", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_call"}} // Action unset
		if !MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected unset action to behave as include")
		}
	})

	t.Run("explicit exclude opts out", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_*", Action: MatcherExclude}}
		if MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected exclude -> false")
		}
	})

	t.Run("last-wins: include then exclude excludes", func(t *testing.T) {
		ms := []*MatcherConfig{
			{Method: "eth_*", Action: MatcherInclude},
			{Method: "eth_sendRawTransaction", Action: MatcherExclude},
		}
		if MatchMatchers(ctx, ms, reqWith("eth_sendRawTransaction", DataFinalityStateUnknown), nil) {
			t.Fatal("expected last matching rule (exclude) to win")
		}
		// A non-excluded eth_ method still matches the include rule.
		if !MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected eth_call to be included")
		}
	})

	t.Run("catch-all exclude base then include (default-exclude shape)", func(t *testing.T) {
		ms := []*MatcherConfig{
			{Method: "*", Action: MatcherExclude}, // evaluated last = fallback
			{Method: "eth_getLogs", Action: MatcherInclude},
		}
		if !MatchMatchers(ctx, ms, reqWith("eth_getLogs", DataFinalityStateUnknown), nil) {
			t.Fatal("expected eth_getLogs included")
		}
		if MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateUnknown), nil) {
			t.Fatal("expected unmatched method to fall through to catch-all exclude")
		}
	})

	t.Run("matches on request params", func(t *testing.T) {
		ms := []*MatcherConfig{{Method: "eth_getBlockByNumber", Params: []interface{}{"latest"}, Action: MatcherInclude}}
		if !MatchMatchers(ctx, ms, reqWith("eth_getBlockByNumber", DataFinalityStateRealtime, "latest", false), nil) {
			t.Fatal("expected params match")
		}
		if MatchMatchers(ctx, ms, reqWith("eth_getBlockByNumber", DataFinalityStateFinalized, "0x1", false), nil) {
			t.Fatal("expected params mismatch -> false")
		}
	})

	t.Run("matches on finality", func(t *testing.T) {
		ms := []*MatcherConfig{{Finality: []DataFinalityState{DataFinalityStateFinalized}, Action: MatcherInclude}}
		if !MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateFinalized), nil) {
			t.Fatal("expected finalized match")
		}
		if MatchMatchers(ctx, ms, reqWith("eth_call", DataFinalityStateRealtime), nil) {
			t.Fatal("expected non-finalized -> false")
		}
	})
}
