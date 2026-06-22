package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// receiptResultBytes builds an eth_getTransactionReceipt result with one log per
// supplied logIndex. Corrupt (underflowed) logIndex values produce longer hex
// strings, which is what made the corrupt response "larger" in the real incident.
func receiptResultBytes(logIndexes []string) []byte {
	logs := make([]string, len(logIndexes))
	for i, li := range logIndexes {
		logs[i] = fmt.Sprintf(`{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"],"data":"0x","logIndex":"%s","removed":false}`, li)
	}
	return []byte(fmt.Sprintf(`{"blockHash":"0x5a28cc00c288af5a055bba9ea5b202b8406e86138ec94ddfc8e96978c752c28a","blockNumber":"0xe57e13","status":"0x1","transactionIndex":"0x0","logs":[%s]}`, strings.Join(logs, ",")))
}

// integrityTestNetwork is a minimal common.Network whose only job is to carry an
// integrity config (the hook reads Config().Integrity). Integrity is opt-in, so
// the wiring test must configure it.
type integrityTestNetwork struct {
	common.Network
	cfg *common.NetworkConfig
}

func (n *integrityTestNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *integrityTestNetwork) Id() string                    { return "evm:123" }
func (n *integrityTestNetwork) ProjectId() string             { return "test-project" }
func (n *integrityTestNetwork) Label() string                 { return "evm:123" }

// indexMagnitudeNetwork enables just the intrinsic logIndex magnitude check.
func indexMagnitudeNetwork() common.Network {
	tru := true
	return &integrityTestNetwork{cfg: &common.NetworkConfig{
		Integrity: &common.IntegrityConfig{
			IntegritySettings: common.IntegritySettings{
				Checks: map[string]*common.IntegrityCheckConfig{"indexMagnitude": {Enabled: &tru}},
			},
		},
	}}
}

var canonicalLogIndexes = []string{"0x0", "0x1", "0x2", "0x3", "0x4", "0x5", "0x6", "0x7", "0x8"}

// 0xfffffff7..0xffffffff == -9..-1 read as int32; the signature of the real bug.
var underflowedLogIndexes = []string{"0xfffffff7", "0xfffffff8", "0xfffffff9", "0xfffffffa", "0xfffffffb", "0xfffffffc", "0xfffffffd", "0xfffffffe", "0xffffffff"}

// TestHandleUpstreamPostForward_LogIndexIntegrity verifies the wiring: the
// post-forward hook runs the unified integrity engine, so a corrupt receipt
// becomes a content-validation error while a canonical receipt and a non-family
// method pass through untouched. (Check-level behavior is covered in the
// integrity package.)
func TestHandleUpstreamPostForward_LogIndexIntegrity(t *testing.T) {
	ctx := context.Background()
	u := common.NewFakeUpstream("test-upstream")

	t.Run("corrupt receipt becomes a content-validation error", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(underflowedLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(ctx, indexMagnitudeNetwork(), u, req, rs, nil, false)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("canonical receipt passes through", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(canonicalLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		gotRs, err := HandleUpstreamPostForward(ctx, indexMagnitudeNetwork(), u, req, rs, nil, false)
		require.NoError(t, err)
		assert.Equal(t, rs, gotRs)
	})

	t.Run("non-family method is not integrity-checked", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"latest"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`{"logIndex":"0xfffffff7"}`), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(ctx, indexMagnitudeNetwork(), u, req, rs, nil, false)
		assert.NoError(t, err)
	})

	t.Run("a rejection increments the integrity_violation metric (check + verdict labels)", func(t *testing.T) {
		// Unique labels so the delta is isolated from other tests.
		labels := []string{"test-project", "", "evm:123", "test-upstream", "eth_gettransactionreceipt", "indexMagnitude", "reject"}
		before := testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...))

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(underflowedLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(ctx, indexMagnitudeNetwork(), u, req, rs, nil, false)
		require.Error(t, err)

		after := testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...))
		assert.Equal(t, before+1, after, "indexMagnitude/reject counter must increment by exactly 1")
	})

	t.Run("opt-in: with no integrity config, a corrupt receipt is not checked", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(underflowedLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		// Network with no Integrity block → nothing runs (safe-by-default deploy).
		noIntegrity := &integrityTestNetwork{cfg: &common.NetworkConfig{}}
		gotRs, err := HandleUpstreamPostForward(ctx, noIntegrity, u, req, rs, nil, false)
		require.NoError(t, err)
		assert.Equal(t, rs, gotRs)
	})
}
