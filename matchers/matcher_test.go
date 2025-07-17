package matchers

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestMatcher_MatchRequest(t *testing.T) {
	tests := []struct {
		name     string
		configs  []*common.MatcherConfig
		network  string
		method   string
		params   []interface{}
		finality common.DataFinalityState
		expected MatchResult
	}{
		{
			name:     "No configs - default include",
			configs:  nil,
			method:   "eth_getLogs",
			expected: MatchResult{Matched: true, Action: common.MatcherInclude},
		},
		{
			name: "Method match - include",
			configs: []*common.MatcherConfig{
				{
					Method: "eth_getLogs",
					Action: common.MatcherInclude,
				},
			},
			method:   "eth_getLogs",
			expected: MatchResult{Matched: true, Action: common.MatcherInclude},
		},
		{
			name: "Method wildcard match",
			configs: []*common.MatcherConfig{
				{
					Method: "eth_*",
					Action: common.MatcherInclude,
				},
			},
			method:   "eth_getBalance",
			expected: MatchResult{Matched: true, Action: common.MatcherInclude},
		},
		{
			name: "Method no match - exclude",
			configs: []*common.MatcherConfig{
				{
					Method: "eth_getLogs",
					Action: common.MatcherInclude,
				},
			},
			method:   "eth_getBalance",
			expected: MatchResult{Matched: false, Action: common.MatcherExclude},
		},
		{
			name: "Finality match",
			configs: []*common.MatcherConfig{
				{
					Method:   "eth_getLogs",
					Finality: common.DataFinalityStateUnfinalized,
					Action:   common.MatcherInclude,
				},
			},
			method:   "eth_getLogs",
			finality: common.DataFinalityStateUnfinalized,
			expected: MatchResult{Matched: true, Action: common.MatcherInclude},
		},
		{
			name: "Network and method match",
			configs: []*common.MatcherConfig{
				{
					Network: "evm:1",
					Method:  "eth_*",
					Action:  common.MatcherInclude,
				},
			},
			network:  "evm:1",
			method:   "eth_getLogs",
			expected: MatchResult{Matched: true, Action: common.MatcherInclude},
		},
		{
			name: "Exclude action",
			configs: []*common.MatcherConfig{
				{
					Method: "debug_*",
					Action: common.MatcherExclude,
				},
			},
			method:   "debug_traceCall",
			expected: MatchResult{Matched: true, Action: common.MatcherExclude},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher := NewMatcher(tt.configs)
			result := matcher.MatchRequest(tt.network, tt.method, tt.params, tt.finality)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMatcher_MatchParams(t *testing.T) {
	tests := []struct {
		name     string
		pattern  []interface{}
		params   []interface{}
		expected bool
	}{
		{
			name:     "Empty pattern matches anything",
			pattern:  []interface{}{},
			params:   []interface{}{"0x123"},
			expected: true,
		},
		{
			name:     "String wildcard match",
			pattern:  []interface{}{"0x*"},
			params:   []interface{}{"0x1234567890"},
			expected: true,
		},
		{
			name: "Object match",
			pattern: []interface{}{
				map[string]interface{}{
					"address": "0x*",
				},
			},
			params: []interface{}{
				map[string]interface{}{
					"address": "0x1234567890",
					"topics":  []interface{}{},
				},
			},
			expected: true,
		},
		{
			name: "Object no match",
			pattern: []interface{}{
				map[string]interface{}{
					"address": "0xabc*",
				},
			},
			params: []interface{}{
				map[string]interface{}{
					"address": "0x1234567890",
				},
			},
			expected: false,
		},
		{
			name:     "Array match",
			pattern:  []interface{}{[]interface{}{"0x*", "latest"}},
			params:   []interface{}{[]interface{}{"0x123", "latest"}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := matchParams(tt.pattern, tt.params)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestConvertLegacyFailsafeConfig(t *testing.T) {
	t.Run("Converts legacy method and finality", func(t *testing.T) {
		cfg := &common.FailsafeConfig{
			MatchMethod:   "eth_getLogs",
			MatchFinality: []common.DataFinalityState{common.DataFinalityStateUnfinalized},
		}

		ConvertLegacyFailsafeConfig(cfg)

		assert.Len(t, cfg.Matchers, 1)
		assert.Equal(t, "eth_getLogs", cfg.Matchers[0].Method)
		assert.Equal(t, common.DataFinalityStateUnfinalized, cfg.Matchers[0].Finality)
		assert.Equal(t, common.MatcherInclude, cfg.Matchers[0].Action)
	})

	t.Run("Preserves existing matchers", func(t *testing.T) {
		cfg := &common.FailsafeConfig{
			MatchMethod: "eth_getLogs",
			Matchers: []*common.MatcherConfig{
				{
					Method: "eth_call",
					Action: common.MatcherInclude,
				},
			},
		}

		ConvertLegacyFailsafeConfig(cfg)

		// Should not change existing matchers
		assert.Len(t, cfg.Matchers, 1)
		assert.Equal(t, "eth_call", cfg.Matchers[0].Method)
	})
}
