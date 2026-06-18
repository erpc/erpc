package integrity

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func receiptResult(logIndexes ...string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}`, li)
	}
	return []byte(fmt.Sprintf(`{"blockHash":"0x5a28","blockNumber":"0xe57e13","status":"0x1","transactionIndex":"0x0","logs":[%s]}`, strings.Join(logs, ",")))
}

func logsResult(logIndexes ...string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","data":"0x","logIndex":"%s"}`, li)
	}
	return []byte("[" + strings.Join(logs, ",") + "]")
}

// run builds a response for method+result and validates it with the given set.
func run(t *testing.T, method string, result []byte, cs CheckSet) error {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":[]}`, method)))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   method,
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
	})
}

func magnitudeOnly() CheckSet { return CheckSet{}.Enable("indexMagnitude", nil) }

func TestValidate_IndexMagnitude(t *testing.T) {
	bad := []string{"0xfffffff7", "0xfffffff8", "0xffffffff"}
	good := []string{"0x0", "0x1", "0x2"}

	tests := []struct {
		name    string
		method  string
		result  []byte
		cs      CheckSet
		wantErr bool
	}{
		{"canonical receipt", "eth_getTransactionReceipt", receiptResult(good...), magnitudeOnly(), false},
		{"underflowed receipt logIndex", "eth_getTransactionReceipt", receiptResult(bad...), magnitudeOnly(), true},
		{"single underflowed among valid", "eth_getTransactionReceipt", receiptResult("0x0", "0x1", "0xfffffff7"), magnitudeOnly(), true},
		{"eth_getLogs valid", "eth_getLogs", logsResult(good...), magnitudeOnly(), false},
		{"eth_getLogs underflowed", "eth_getLogs", logsResult("0x0", "0xfffffffe"), magnitudeOnly(), true},
		{"empty logs array", "eth_getLogs", []byte("[]"), magnitudeOnly(), false},
		{"null result", "eth_getTransactionReceipt", []byte("null"), magnitudeOnly(), false},
		{"check disabled → corrupt passes", "eth_getTransactionReceipt", receiptResult(bad...), CheckSet{}, false},
		{"non-family method → no checks", "eth_call", []byte(`{"logIndex":"0xfffffff7"}`), magnitudeOnly(), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t, tc.method, tc.result, tc.cs)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation),
					"expected ErrEndpointContentValidation, got %v", err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidate_TransactionIndexMagnitude(t *testing.T) {
	// A receipt whose transactionIndex itself is underflowed must be rejected.
	result := []byte(`{"blockNumber":"0xe57e13","transactionIndex":"0xfffffffe","logs":[]}`)
	err := run(t, "eth_getTransactionReceipt", result, magnitudeOnly())
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation))
}

func TestIsImplausibleIndex(t *testing.T) {
	assert.False(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmIndex-1)))
	assert.True(t, isImplausibleIndex(fmt.Sprintf("0x%x", maxPlausibleEvmIndex)))
	assert.True(t, isImplausibleIndex("0xfffffff7")) // the real-incident value
	assert.False(t, isImplausibleIndex("0x0"))
	assert.False(t, isImplausibleIndex("0x8"))
	assert.False(t, isImplausibleIndex(""))        // absent → not implausible
	assert.False(t, isImplausibleIndex("not-hex")) // unparseable → not flagged
}

func TestDecoded_SharesOneParseAcrossViews(t *testing.T) {
	// For getBlockReceipts, Logs() is derived from Receipts(); both must reflect
	// the same single decode.
	d := newDecoded(MethodGetBlockReceipts, []byte(`[{"transactionIndex":"0x0","logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]}]`))
	require.Len(t, d.Receipts(), 1)
	require.Len(t, d.Logs(), 2)
	assert.Equal(t, "0x1", d.Logs()[1].LogIndex)
}
