package evm

import (
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestBatchingKey(t *testing.T) {
	key1 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}
	key2 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}
	key3 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "12345",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}

	require.Equal(t, key1.String(), key2.String())
	require.NotEqual(t, key1.String(), key3.String())
}

func TestDirectivesKeyDerivation(t *testing.T) {
	dirs := &common.RequestDirectives{}
	dirs.UseUpstream = "alchemy"
	dirs.SkipCacheRead = true
	dirs.RetryEmpty = true

	key := DeriveDirectivesKey(dirs)
	require.Contains(t, key, "use-upstream=alchemy")
	require.Contains(t, key, "skip-cache-read=true")
	require.Contains(t, key, "retry-empty=true")
}

func TestCallKeyDerivation(t *testing.T) {
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1234567890123456789012345678901234567890",
			"data": "0xabcdef",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	key, err := DeriveCallKey(req)
	require.NoError(t, err)
	require.NotEmpty(t, key)

	// Same request should produce same key
	key2, err := DeriveCallKey(req)
	require.NoError(t, err)
	require.Equal(t, key, key2)
}

func TestDirectivesKeyDerivation_Nil(t *testing.T) {
	key := DeriveDirectivesKey(nil)
	require.Equal(t, "v1:", key)
}

func TestDirectivesKeyDerivation_VersionPrefix(t *testing.T) {
	dirs := &common.RequestDirectives{}
	dirs.UseUpstream = "alchemy"

	key := DeriveDirectivesKey(dirs)
	require.True(t, strings.HasPrefix(key, "v1:"), "key should have version prefix")
}

func TestCallKeyDerivation_NilRequest(t *testing.T) {
	key, err := DeriveCallKey(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request is nil")
	require.Empty(t, key)
}

func TestIsEligibleForBatching(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                 true,
		AllowPendingTagBatching: false,
	}
	cfg.SetDefaults()

	tests := []struct {
		name     string
		method   string
		params   []interface{}
		eligible bool
		reason   string
	}{
		{
			name:   "eligible basic eth_call",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"latest",
			},
			eligible: true,
		},
		{
			name:   "eligible with finalized tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"finalized",
			},
			eligible: true,
		},
		{
			name:   "ineligible - pending tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"pending",
			},
			eligible: false,
			reason:   "pending tag not allowed",
		},
		{
			name:   "ineligible - has from field",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0x1234567890123456789012345678901234567890",
					"data": "0xabcd",
					"from": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				"latest",
			},
			eligible: false,
			reason:   "has from field",
		},
		{
			name:   "ineligible - has value field",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":    "0x1234567890123456789012345678901234567890",
					"data":  "0xabcd",
					"value": "0x1",
				},
				"latest",
			},
			eligible: false,
			reason:   "has value field",
		},
		{
			name:   "ineligible - has state override (3rd param)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"latest",
				map[string]interface{}{}, // state override
			},
			eligible: false,
			reason:   "has state override",
		},
		{
			name:     "ineligible - not eth_call",
			method:   "eth_getBalance",
			params:   []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			eligible: false,
			reason:   "not eth_call",
		},
		{
			name:   "ineligible - already multicall (recursion guard)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0xcA11bde05977b3631167028862bE2a173976CA11", // multicall3 address
					"data": "0x82ad56cb",                                 // aggregate3 selector
				},
				"latest",
			},
			eligible: false,
			reason:   "already multicall",
		},
		{
			name:   "eligible with safe tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"safe",
			},
			eligible: true,
		},
		{
			name:   "eligible with earliest tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"earliest",
			},
			eligible: true,
		},
		{
			name:   "eligible with numeric block number (hex)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"0x1234",
			},
			eligible: true,
		},
		{
			name:   "eligible with block hash",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef", // 32-byte hash
			},
			eligible: true,
		},
		{
			name:   "ineligible - unknown block tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"unknown_tag",
			},
			eligible: false,
			reason:   "block tag not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jrq := common.NewJsonRpcRequest(tt.method, tt.params)
			req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

			eligible, reason := IsEligibleForBatching(req, cfg)
			require.Equal(t, tt.eligible, eligible, "reason: %s", reason)
			if !tt.eligible {
				require.Contains(t, reason, tt.reason)
			}
		})
	}
}
