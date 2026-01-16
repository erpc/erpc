package erpc

import (
	"encoding/json"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectEthCallBatchInfo(t *testing.T) {
	buildRaw := func(t *testing.T, method string, params []interface{}, networkId string) json.RawMessage {
		t.Helper()
		body := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		}
		if networkId != "" {
			body["networkId"] = networkId
		}
		raw, err := common.SonicCfg.Marshal(body)
		require.NoError(t, err)
		return raw
	}

	callObj := map[string]interface{}{"to": "0x0000000000000000000000000000000000000001"}

	cases := []struct {
		name         string
		requests     []json.RawMessage
		arch         string
		chainId      string
		wantNil      bool
		wantErr      bool
		wantNetwork  string
		wantBlockRef string
		wantBlock    interface{}
	}{
		{
			name:     "single request",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1")},
			wantNil:  true,
		},
		{
			name:     "non-evm arch",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1")},
			arch:     "solana",
			wantNil:  true,
		},
		{
			name:     "invalid json",
			requests: []json.RawMessage{json.RawMessage("{"), buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1")},
			wantNil:  true,
			wantErr:  true,
		},
		{
			name:     "non-eth_call",
			requests: []json.RawMessage{buildRaw(t, "eth_getBalance", []interface{}{callObj}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1")},
			wantNil:  true,
		},
		{
			name:     "missing network",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj}, ""), buildRaw(t, "eth_call", []interface{}{callObj}, "")},
			wantNil:  true,
		},
		{
			name:     "network mismatch",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj}, "evm:2")},
			wantNil:  true,
		},
		{
			name:     "block mismatch",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj, "0x1"}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj, "0x2"}, "evm:1")},
			wantNil:  true,
		},
		{
			name:     "invalid block param",
			requests: []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0xzz"}}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj, "latest"}, "evm:1")},
			wantNil:  true,
			wantErr:  true,
		},
		{
			name:         "success explicit network",
			requests:     []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj, "0x1"}, "evm:1"), buildRaw(t, "eth_call", []interface{}{callObj, "0x1"}, "evm:1")},
			wantNetwork:  "evm:1",
			wantBlockRef: "1",
			wantBlock:    "0x1",
		},
		{
			name:         "success default network",
			requests:     []json.RawMessage{buildRaw(t, "eth_call", []interface{}{callObj}, ""), buildRaw(t, "eth_call", []interface{}{callObj}, "")},
			arch:         "evm",
			chainId:      "123",
			wantNetwork:  "evm:123",
			wantBlockRef: "latest",
			wantBlock:    "latest",
		},
		{
			name: "mixed requireCanonical - one false one true",
			requests: []json.RawMessage{
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": false}}, "evm:1"),
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true}}, "evm:1"),
			},
			wantNil: true,
		},
		{
			name: "mixed requireCanonical - one false one default",
			requests: []json.RawMessage{
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": false}}, "evm:1"),
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234"}}, "evm:1"),
			},
			wantNil: true,
		},
		{
			name: "same requireCanonical - both true",
			requests: []json.RawMessage{
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true}}, "evm:1"),
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true}}, "evm:1"),
			},
			wantNetwork:  "evm:1",
			wantBlockRef: "0x1234567890123456789012345678901234567890123456789012345678901234",
			wantBlock:    map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true},
		},
		{
			name: "same requireCanonical - explicit true and default compatible",
			requests: []json.RawMessage{
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true}}, "evm:1"),
				buildRaw(t, "eth_call", []interface{}{callObj, map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234"}}, "evm:1"),
			},
			wantNetwork:  "evm:1",
			wantBlockRef: "0x1234567890123456789012345678901234567890123456789012345678901234",
			wantBlock:    map[string]interface{}{"blockHash": "0x1234567890123456789012345678901234567890123456789012345678901234", "requireCanonical": true},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			info, err := detectEthCallBatchInfo(tt.requests, tt.arch, tt.chainId)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantNil {
				assert.Nil(t, info)
				return
			}
			require.NotNil(t, info)
			assert.Equal(t, tt.wantNetwork, info.networkId)
			assert.Equal(t, tt.wantBlockRef, info.blockRef)
			assert.Equal(t, tt.wantBlock, info.blockParam)
		})
	}
}
