package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetByPath_CopyOnWrite(t *testing.T) {
	stateOverride := map[string]interface{}{
		"0xabc": map[string]interface{}{"balance": "0x1"},
	}
	filter := map[string]interface{}{
		"fromBlock":     "0x01",
		"toBlock":       "0x02",
		"stateOverride": stateOverride,
	}
	params := []interface{}{filter}

	updatedRoot, changed := setByPath(params, []interface{}{0, "fromBlock"}, "0x1")
	require.True(t, changed)

	updatedParams, ok := updatedRoot.([]interface{})
	require.True(t, ok)

	updatedFilter, ok := updatedParams[0].(map[string]interface{})
	require.True(t, ok)

	// Leaf is updated only in cloned branch.
	assert.Equal(t, "0x1", updatedFilter["fromBlock"])
	assert.Equal(t, "0x01", filter["fromBlock"])

	// Path container was cloned (mutating updated filter does not touch original filter).
	updatedFilter["toBlock"] = "0x99"
	assert.Equal(t, "0x02", filter["toBlock"])

	// Sibling branch stays shared (mutating stateOverride in new tree affects original branch too).
	updatedStateOverride, ok := updatedFilter["stateOverride"].(map[string]interface{})
	require.True(t, ok)
	updatedStateOverride["0xdef"] = map[string]interface{}{"nonce": "0x2"}
	_, exists := stateOverride["0xdef"]
	assert.True(t, exists)
}

func TestNormalizeHttpJsonRpc_UsesCopyOnWriteWithoutDeepCopyingSiblings(t *testing.T) {
	stateOverride := map[string]interface{}{
		"0xabc": map[string]interface{}{"balance": "0x1"},
	}
	filter := map[string]interface{}{
		"fromBlock":     "0x01",
		"toBlock":       "0x02",
		"stateOverride": stateOverride,
	}
	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{filter})
	nrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	NormalizeHttpJsonRpc(context.Background(), nrq, jrq)

	params := jrq.Params
	require.Len(t, params, 1)

	newFilter, ok := params[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "0x1", newFilter["fromBlock"])
	assert.Equal(t, "0x2", newFilter["toBlock"])

	// Original top-level filter is unchanged.
	assert.Equal(t, "0x01", filter["fromBlock"])
	assert.Equal(t, "0x02", filter["toBlock"])

	// stateOverride is a sibling branch and should stay shared.
	newStateOverride, ok := newFilter["stateOverride"].(map[string]interface{})
	require.True(t, ok)
	newStateOverride["0xdef"] = map[string]interface{}{"nonce": "0x2"}
	_, exists := stateOverride["0xdef"]
	assert.True(t, exists)
}

func BenchmarkReplaceParamAtPath_CopyOnWriteVsDeepCopy(b *testing.B) {
	params := benchmarkLargeParams()
	path := []interface{}{0, "fromBlock"}
	value := "0x1"

	b.Run("copy_on_write", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			updated, ok := replaceParamAtPath(params, path, value)
			if !ok {
				b.Fatal("expected replacement")
			}
			benchmarkParamsSink = updated
		}
	})

	b.Run("deep_copy_then_replace", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cloned := benchmarkDeepCopyParams(params)
			updated, ok := replaceParamAtPath(cloned, path, value)
			if !ok {
				b.Fatal("expected replacement")
			}
			benchmarkParamsSink = updated
		}
	})
}

var benchmarkParamsSink []interface{}

func benchmarkLargeParams() []interface{} {
	stateOverride := make(map[string]interface{}, 256)
	for i := 0; i < 256; i++ {
		k := "0x" + string(rune('a'+(i%26))) + string(rune('a'+((i/26)%26)))
		stateOverride[k] = map[string]interface{}{
			"balance": "0x123456",
			"nonce":   "0x7",
			"code":    "0x6001600055",
		}
	}
	return []interface{}{
		map[string]interface{}{
			"fromBlock":     "0x01",
			"toBlock":       "0x02",
			"stateOverride": stateOverride,
		},
	}
}

func benchmarkDeepCopyParams(params []interface{}) []interface{} {
	if params == nil {
		return nil
	}
	result := make([]interface{}, len(params))
	for i, param := range params {
		result[i] = benchmarkDeepCopyValue(param)
	}
	return result
}

func benchmarkDeepCopyValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case map[string]interface{}:
		m := make(map[string]interface{}, len(val))
		for k, child := range val {
			m[k] = benchmarkDeepCopyValue(child)
		}
		return m
	case []interface{}:
		s := make([]interface{}, len(val))
		for i, child := range val {
			s[i] = benchmarkDeepCopyValue(child)
		}
		return s
	default:
		return val
	}
}
