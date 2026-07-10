package upstream

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/thirdparty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func creditReq(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`,
	))
}

// Pricing is the vendor's call: attemptCreditUnits delegates to the
// vendor's CreditUnits (its published table merged with the operator's
// providers[].settings.creditUnits → UpstreamConfig.CreditUnits override);
// vendors without pricing cost a flat 1 credit per request unless the
// config prices or opts them out.
func TestAttemptCreditUnits_VendorOwnedPricing(t *testing.T) {
	alchemy := thirdparty.NewVendorsRegistry().LookupByName("alchemy")
	require.NotNil(t, alchemy)

	u := &Upstream{
		vendor: alchemy,
		config: &common.UpstreamConfig{CreditUnits: map[string]int64{"eth_getLogs": 80}},
	}
	assert.Equal(t, int64(80), u.attemptCreditUnits(creditReq(t, "eth_getLogs")), "operator override wins")
	assert.Equal(t, int64(26), u.attemptCreditUnits(creditReq(t, "eth_call")), "vendor table survives a partial override")
	assert.Equal(t, int64(20), u.attemptCreditUnits(creditReq(t, "erpc_totallyUnknown")), "vendor '*' fallback covers unlisted methods")

	// No vendor pricing, no config → flat 1 credit per request: an
	// unknown or self-hosted vendor still costs one request.
	bare := &Upstream{config: &common.UpstreamConfig{}}
	assert.Equal(t, int64(1), bare.attemptCreditUnits(creditReq(t, "eth_call")))

	// No vendor pricing but the config prices it.
	custom := &Upstream{config: &common.UpstreamConfig{CreditUnits: map[string]int64{"*": 3}}}
	assert.Equal(t, int64(3), custom.attemptCreditUnits(creditReq(t, "eth_call")))

	// Explicit opt-out: "*": 0 silences a vendor entirely.
	optedOut := &Upstream{config: &common.UpstreamConfig{CreditUnits: map[string]int64{"*": 0}}}
	assert.Equal(t, int64(0), optedOut.attemptCreditUnits(creditReq(t, "eth_call")))
}

func TestResolveCreditUnits_Precedence(t *testing.T) {
	defaults := map[string]int64{"eth_call": 26, "*": 20}
	override := map[string]int64{"eth_call": 30, "eth_getLogs": 80}

	assert.Equal(t, int64(30), common.ResolveCreditUnits(defaults, override, "eth_call"), "override method beats defaults")
	assert.Equal(t, int64(80), common.ResolveCreditUnits(defaults, override, "eth_getLogs"), "override adds methods")
	assert.Equal(t, int64(20), common.ResolveCreditUnits(defaults, override, "eth_unlisted"), "defaults '*' fallback")
	assert.Equal(t, int64(5), common.ResolveCreditUnits(nil, map[string]int64{"*": 5}, "eth_call"), "override '*' without defaults")
	assert.Equal(t, int64(1), common.ResolveCreditUnits(nil, nil, "eth_call"), "flat 1 when nothing prices the method")
}

func TestVendorSettings_CreditUnitsExtraction(t *testing.T) {
	// YAML decodes nested numbers as int or float64 — both must normalize.
	settings := common.VendorSettings{
		"chainsUrl": "https://example.com",
		"creditUnits": map[string]interface{}{
			"eth_getLogs": 80,
			"eth_call":    float64(30),
			"*":           int64(5),
		},
	}
	units := settings.CreditUnits()
	require.NotNil(t, units)
	assert.Equal(t, int64(80), units["eth_getLogs"])
	assert.Equal(t, int64(30), units["eth_call"])
	assert.Equal(t, int64(5), units["*"])

	assert.Nil(t, common.VendorSettings{}.CreditUnits())
	assert.Nil(t, common.VendorSettings{"creditUnits": "bogus"}.CreditUnits())
}
