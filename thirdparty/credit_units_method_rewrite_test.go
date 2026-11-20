package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// A vendor bills the method it ran: with methodRewrite, the credit cost of
// a client method is its target's, built-in table and overrides alike.
func TestCreditUnits_UseRewrittenMethod(t *testing.T) {
	type cuVendor interface {
		CreditUnits(*common.NormalizedRequest, *common.UpstreamConfig) int64
	}
	// target is a method each vendor's built-in table prices apart from
	// its default, which an unknown custom method falls to.
	vendors := map[string]struct {
		v      cuVendor
		target string
	}{
		"alchemy":   {CreateAlchemyVendor().(*AlchemyVendor), "eth_chainId"},
		"drpc":      {CreateDrpcVendor().(*DrpcVendor), "eth_chainId"},
		"quicknode": {CreateQuicknodeVendor().(*QuicknodeVendor), "trace_filter"},
	}
	for name, tc := range vendors {
		t.Run(name, func(t *testing.T) {
			v, target := tc.v, tc.target
			plain := &common.UpstreamConfig{}
			assert.NotEqual(t, v.CreditUnits(cuReq(t, target), plain), v.CreditUnits(cuReq(t, "custom_send"), plain),
				"precondition: target and client method cost differently")

			renamed := &common.UpstreamConfig{MethodRewrite: map[string]string{"custom_send": target}}
			assert.Equal(t, v.CreditUnits(cuReq(t, target), renamed), v.CreditUnits(cuReq(t, "custom_send"), renamed),
				"built-in table: the renamed method costs its target")

			override := &common.UpstreamConfig{
				MethodRewrite: map[string]string{"custom_send": target},
				CreditUnits:   map[string]int64{target: 77},
			}
			assert.Equal(t, int64(77), v.CreditUnits(cuReq(t, "custom_send"), override),
				"creditUnits overrides are keyed by the target method")
		})
	}
}
