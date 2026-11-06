package evm

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	word2a = "0x000000000000000000000000000000000000000000000000000000000000002a"
	word01 = "0x0000000000000000000000000000000000000000000000000000000000000001"
	word00 = "0x0000000000000000000000000000000000000000000000000000000000000000"
	addrA  = "0x00000000000000000000000000000000000000aa"
)

func decodeParams(t *testing.T, js string) []interface{} {
	t.Helper()
	var p []interface{}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(js), &p))
	return p
}

func encode(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := common.SonicCfg.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestFormatRequestParams_Canonical(t *testing.T) {
	canonical := common.EvmRequestFormatCanonical

	t.Run("EthCallPadsStorageKeysAndValues", func(t *testing.T) {
		js := `[{"to":"` + addrA + `"},"latest",{"` + addrA + `":{"state":{"0x2a":"0x1"},"stateDiff":{"0x1":"0x0"}}}]`
		in := decodeParams(t, js)
		out, outcome, err := FormatRequestParams(canonical, "eth_call", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatRewritten, outcome)
		acc := out[2].(map[string]interface{})[addrA].(map[string]interface{})
		assert.Equal(t, map[string]interface{}{word2a: word01}, acc["state"])
		assert.Equal(t, map[string]interface{}{word01: word00}, acc["stateDiff"])
		assert.Equal(t, decodeParams(t, js), in, "the caller's params are never modified")
	})

	t.Run("EveryMappedPath", func(t *testing.T) {
		ov := `{"` + addrA + `":{"state":{"0x2a":"0x1"}}}`
		cases := map[string]string{
			"eth_estimateGas":      `[{},"latest",` + ov + `]`,
			"eth_createAccessList": `[{},"latest",` + ov + `]`,
			"eth_callMany":         `[[{"transactions":[{}]}],{},` + ov + `]`,
			"eth_simulateV1":       `[{"blockStateCalls":[{"calls":[]},{"stateOverrides":` + ov + `}]},"latest"]`,
			"debug_traceCall":      `[{},"latest",{"stateOverrides":` + ov + `}]`,
			"debug_traceCallMany":  `[[{"transactions":[{}]}],{},{"stateOverrides":` + ov + `}]`,
			"trace_call":           `[{},["trace"],"latest",` + ov + `]`,
		}
		for method, js := range cases {
			out, outcome, err := FormatRequestParams(canonical, method, decodeParams(t, js))
			require.NoError(t, err, method)
			assert.Equal(t, RequestFormatRewritten, outcome, method)
			s := encode(t, out)
			assert.Contains(t, s, `"`+word2a+`":"`+word01+`"`, method)
			assert.NotContains(t, s, `"0x2a"`, method)
		}
	})

	t.Run("AlreadyCanonicalIsUnchanged", func(t *testing.T) {
		in := decodeParams(t, `[{},"latest",{"`+addrA+`":{"state":{"`+word2a+`":"`+word01+`"}}}]`)
		out, outcome, err := FormatRequestParams(canonical, "eth_call", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
		assert.Equal(t, in, out)
	})

	t.Run("UnknownMethodPassesThrough", func(t *testing.T) {
		in := decodeParams(t, `[{"`+addrA+`":{"state":{"0x2a":"0x1"}}}]`)
		out, outcome, err := FormatRequestParams(canonical, "vendor_futureCall", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
		assert.Equal(t, in, out)
	})

	t.Run("UnsetFormatPassesThrough", func(t *testing.T) {
		in := decodeParams(t, `[{},"latest",{"`+addrA+`":{"state":{"0x2a":"0x1"}}}]`)
		_, outcome, err := FormatRequestParams("", "eth_call", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
	})

	t.Run("CanonicalNeverRefusesNorRenames", func(t *testing.T) {
		js := `[[{"transactions":[{}],"blockOverride":{"number":"0x5","prevRandao":"0x1"}}],{},{"` + addrA + `":{"movePrecompileToAddress":"0x01"}}]`
		_, outcome, err := FormatRequestParams(canonical, "eth_callMany", decodeParams(t, js))
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
	})

	unreadable := map[string]string{
		"WordLongerThan32Bytes": `{"0x2a":"0x1` + word00[2:] + `"}`,
		"DecimalString":         `{"0x2a":"42"}`,
		"NonHex":                `{"0x2a":"0xzz"}`,
		"BareZeroX":             `{"0x2a":"0x"}`,
		"JsonNumber":            `{"0x2a":42}`,
		"KeysEqualAfterPadding": `{"0x1":"0x1","0x01":"0x2"}`,
		"StateNotAMap":          `["0x1"]`,
	}
	for name, state := range unreadable {
		t.Run("Unreadable"+name+"SendsWholeRequestUnchanged", func(t *testing.T) {
			// One readable slot next to the unreadable one: nothing is rewritten.
			js := `[{},"latest",{"` + addrA + `":{"state":` + state + `},"0x00000000000000000000000000000000000000bb":{"state":{"0x2a":"0x1"}}}]`
			in := decodeParams(t, js)
			out, outcome, err := FormatRequestParams(canonical, "eth_call", in)
			require.NoError(t, err)
			assert.Equal(t, RequestFormatUnreadable, outcome)
			assert.Equal(t, in, out)
		})
	}

	t.Run("NonMapOverridePositionIsNotAnOverride", func(t *testing.T) {
		// Erigon's eth_createAccessList takes an optimizeGas bool at [2].
		_, outcome, err := FormatRequestParams(canonical, "eth_createAccessList", decodeParams(t, `[{},"latest",true]`))
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
	})
}

func TestFormatRequestParams_ErigonLegacy(t *testing.T) {
	legacy := common.EvmRequestFormatErigonLegacy

	t.Run("PadsKeysAndMakesValuesMinimal", func(t *testing.T) {
		in := decodeParams(t, `[{},"latest",{"`+addrA+`":{"state":{"0x2a":"`+word01+`","0x1":"0x00"}}}]`)
		out, outcome, err := FormatRequestParams(legacy, "eth_call", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatRewritten, outcome)
		acc := out[2].(map[string]interface{})[addrA].(map[string]interface{})
		assert.Equal(t, map[string]interface{}{word2a: "0x1", word01: "0x0"}, acc["state"])
	})

	t.Run("RefusesAccountFieldErigonIgnores", func(t *testing.T) {
		in := decodeParams(t, `[{},"latest",{"`+addrA+`":{"movePrecompileToAddress":"0x00000000000000000000000000000000000000cc"}}]`)
		_, outcome, err := FormatRequestParams(legacy, "eth_call", in)
		assert.Equal(t, RequestFormatRefused, outcome)
		assert.ErrorContains(t, err, "movePrecompileToAddress")
	})

	t.Run("RenamesCallManyBlockOverride", func(t *testing.T) {
		in := decodeParams(t, `[[{"transactions":[{}],"blockOverride":{"number":"0x5","time":"0x6","feeRecipient":"`+addrA+`","baseFeePerGas":"0x7","gasLimit":"0x8"}}],{"blockNumber":"latest"}]`)
		out, outcome, err := FormatRequestParams(legacy, "eth_callMany", in)
		require.NoError(t, err)
		assert.Equal(t, RequestFormatRewritten, outcome)
		bo := out[0].([]interface{})[0].(map[string]interface{})["blockOverride"]
		assert.Equal(t, map[string]interface{}{
			"blockNumber": "0x5", "timestamp": "0x6", "coinbase": addrA, "baseFee": "0x7", "gasLimit": "0x8",
		}, bo)
	})

	t.Run("RefusesBlockOverrideFieldErigonLacks", func(t *testing.T) {
		in := decodeParams(t, `[[{"transactions":[{}],"blockOverride":{"prevRandao":"0x1"}}],{}]`)
		_, outcome, err := FormatRequestParams(legacy, "debug_traceCallMany", in)
		assert.Equal(t, RequestFormatRefused, outcome)
		assert.ErrorContains(t, err, "prevRandao")
	})

	t.Run("RefusesBlockOverrideGivenTwice", func(t *testing.T) {
		in := decodeParams(t, `[[{"transactions":[{}],"blockOverride":{"number":"0x5","blockNumber":"0x6"}}],{}]`)
		_, outcome, _ := FormatRequestParams(legacy, "eth_callMany", in)
		assert.Equal(t, RequestFormatRefused, outcome)
	})

	t.Run("TraceCallInputBecomesData", func(t *testing.T) {
		out, outcome, err := FormatRequestParams(legacy, "trace_call", decodeParams(t, `[{"to":"`+addrA+`","input":"0x0102"},["trace"],"latest"]`))
		require.NoError(t, err)
		assert.Equal(t, RequestFormatRewritten, outcome)
		assert.Equal(t, map[string]interface{}{"to": addrA, "data": "0x0102"}, out[0])
	})

	t.Run("TraceCallManyInputBecomesData", func(t *testing.T) {
		out, outcome, err := FormatRequestParams(legacy, "trace_callMany", decodeParams(t, `[[[{"input":"0x0102"},["trace"]]],"latest"]`))
		require.NoError(t, err)
		assert.Equal(t, RequestFormatRewritten, outcome)
		assert.Equal(t, map[string]interface{}{"data": "0x0102"}, out[0].([]interface{})[0].([]interface{})[0])
	})

	t.Run("TraceCallEqualInputAndDataUnchanged", func(t *testing.T) {
		_, outcome, err := FormatRequestParams(legacy, "trace_call", decodeParams(t, `[{"input":"0x01","data":"0x01"},["trace"],"latest"]`))
		require.NoError(t, err)
		assert.Equal(t, RequestFormatUnchanged, outcome)
	})

	t.Run("TraceCallUnequalInputAndDataRefused", func(t *testing.T) {
		_, outcome, err := FormatRequestParams(legacy, "trace_call", decodeParams(t, `[{"input":"0x01","data":"0x02"},["trace"],"latest"]`))
		assert.Equal(t, RequestFormatRefused, outcome)
		assert.Error(t, err)
	})

	t.Run("RefusalWinsOverUnreadable", func(t *testing.T) {
		in := decodeParams(t, `[{},"latest",{"`+addrA+`":{"state":{"0x2a":"42"},"movePrecompileToAddress":"0x01"}}]`)
		_, outcome, _ := FormatRequestParams(legacy, "eth_call", in)
		assert.Equal(t, RequestFormatRefused, outcome)
	})
}
