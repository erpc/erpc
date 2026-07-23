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

// dRPC prices flat: 20 CU for every billable method (debug/trace included,
// no multiplier tier), 0 CU for informational methods, default 20 for
// unlisted — overridable per method via the config.
func TestAttemptCreditUnits_Drpc(t *testing.T) {
	drpc := thirdparty.NewVendorsRegistry().LookupByName("drpc")
	require.NotNil(t, drpc)

	u := &Upstream{vendor: drpc, config: &common.UpstreamConfig{}}
	assert.Equal(t, int64(20), u.attemptCreditUnits(creditReq(t, "eth_call")), "flat 20")
	assert.Equal(t, int64(20), u.attemptCreditUnits(creditReq(t, "eth_getLogs")), "no getLogs surcharge")
	assert.Equal(t, int64(20), u.attemptCreditUnits(creditReq(t, "debug_traceTransaction")), "no advanced-API multiplier")
	assert.Equal(t, int64(0), u.attemptCreditUnits(creditReq(t, "eth_chainId")), "informational method is free")
	assert.Equal(t, int64(0), u.attemptCreditUnits(creditReq(t, "net_version")), "informational method is free")
	assert.Equal(t, int64(20), u.attemptCreditUnits(creditReq(t, "erpc_totallyUnknown")), "default '*' fallback")

	// Operator override wins per method over the flat table.
	over := &Upstream{vendor: drpc, config: &common.UpstreamConfig{CreditUnits: map[string]int64{"eth_getLogs": 99}}}
	assert.Equal(t, int64(99), over.attemptCreditUnits(creditReq(t, "eth_getLogs")))
	assert.Equal(t, int64(20), over.attemptCreditUnits(creditReq(t, "eth_call")), "unoverridden method keeps the flat table")
}

// rateLimitHits resolves the per-call budget weight: a flat 1 in the default
// request-count mode, or the pre-flight estimated CU when the upstream opts
// into credit counting (0-CU methods consume nothing).
func TestRateLimitHits_CountMode(t *testing.T) {
	alchemy := thirdparty.NewVendorsRegistry().LookupByName("alchemy")
	require.NotNil(t, alchemy)

	// Default (request) mode: always 1, regardless of method cost.
	reqMode := &Upstream{vendor: alchemy, config: &common.UpstreamConfig{}}
	assert.Equal(t, uint32(1), reqMode.rateLimitHits(creditReq(t, "eth_getLogs")))
	assert.Equal(t, uint32(1), reqMode.rateLimitHits(creditReq(t, "eth_chainId")))

	// Credit mode: the pre-flight estimated CU is the hit weight.
	creditMode := &Upstream{vendor: alchemy, config: &common.UpstreamConfig{RateLimitCountMode: common.RateLimitCountModeCredit}}
	assert.Equal(t, uint32(60), creditMode.rateLimitHits(creditReq(t, "eth_getLogs")))
	assert.Equal(t, uint32(26), creditMode.rateLimitHits(creditReq(t, "eth_call")))
	assert.Equal(t, uint32(0), creditMode.rateLimitHits(creditReq(t, "eth_chainId")), "0-CU method consumes nothing")
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
