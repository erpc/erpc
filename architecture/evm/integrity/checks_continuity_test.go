package integrity

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockHistory map[int64]string

func (m mockHistory) HashAt(number int64) (string, bool) { h, ok := m[number]; return h, ok }

func blockResult(number, hash, parentHash string) []byte {
	return []byte(fmt.Sprintf(`{"number":"%s","hash":"%s","parentHash":"%s"}`, number, hash, parentHash))
}

func validateBlock(t *testing.T, result []byte, cs CheckSet, hist History, resolver Resolver) Result {
	t.Helper()
	return validateBlockVia(t, "eth_getBlockByNumber", result, cs, hist, resolver)
}

func validateBlockVia(t *testing.T, method string, result []byte, cs CheckSet, hist History, resolver Resolver) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":["0x1",false]}`, method)))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	in := Input{
		Method:   method,
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Reorg:    DefaultReorgPolicy(),
	}
	if hist != nil {
		in.History = hist
	}
	if resolver != nil {
		in.Resolver = resolver
	}
	return Validate(context.Background(), in)
}

// finalized is a resolver that reports every block as finalized.
var finalized Resolver = mockResolver{finalized: true, known: true}

func TestCheck_ParentHashLinkage(t *testing.T) {
	cs := only("parentHashLinkage", nil)
	hist := mockHistory{0x10: "0xaaa"} // observed block 16 → 0xaaa

	t.Run("matching parent links cleanly", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xbbb", "0xaaa"), cs, hist, nil)
		assert.NoError(t, res.Err)
	})
	t.Run("broken link is rejected on a finalized block", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xbbb", "0xccc"), cs, hist, finalized)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	})
	t.Run("broken link is recorded (not rejected) on an unfinalized block", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xbbb", "0xccc"), cs, hist, nil)
		assert.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
		assert.Equal(t, "parentHashLinkage", res.Recorded[0].CheckID)
	})
	t.Run("unobserved parent → skip", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x50", "0xbbb", "0xccc"), cs, hist, finalized)
		assert.NoError(t, res.Err)
		assert.Empty(t, res.Recorded)
	})
	t.Run("no history → skip", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xbbb", "0xccc"), cs, nil, finalized)
		assert.NoError(t, res.Err)
	})
}

func TestCheck_HashStability(t *testing.T) {
	cs := only("hashStability", nil)
	hist := mockHistory{0x11: "0xbbb"} // observed block 17 → 0xbbb

	t.Run("same hash is stable", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xbbb", "0xaaa"), cs, hist, finalized)
		assert.NoError(t, res.Err)
	})
	t.Run("changed hash on a finalized block is corruption → reject", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xddd", "0xaaa"), cs, hist, finalized)
		require.Error(t, res.Err)
	})
	t.Run("changed hash on an unfinalized block is a reorg → record", func(t *testing.T) {
		res := validateBlock(t, blockResult("0x11", "0xddd", "0xaaa"), cs, hist, nil)
		assert.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
	})
}

// The byHashRequests param exempts explicit by-hash lookups (a client asking
// for an exact hash may legitimately receive an orphaned block) while keeping
// by-number continuity strict. Default is byte-identical to prior behavior.
func TestCheck_Continuity_ByHashRequestsParam(t *testing.T) {
	hist := mockHistory{0x10: "0xaaa", 0x11: "0xbbb"} // pins: 16→0xaaa, 17→0xbbb
	orphan := blockResult("0x11", "0xddd", "0xccc")   // both checks would fire: hash≠pin(17), parent≠pin(16)
	skipParams := map[string]string{common.IntegrityParamByHashRequests: "skip"}

	for _, id := range []string{"hashStability", "parentHashLinkage"} {
		t.Run(id, func(t *testing.T) {
			t.Run("by-hash lookup with byHashRequests:skip → skipped, served", func(t *testing.T) {
				res := validateBlockVia(t, "eth_getBlockByHash", orphan, only(id, skipParams), hist, finalized)
				assert.NoError(t, res.Err)
				assert.Empty(t, res.Recorded)
				assert.Equal(t, "skip", outcomeOf(res, id))
			})
			t.Run("by-number lookup stays strict with byHashRequests:skip", func(t *testing.T) {
				res := validateBlockVia(t, "eth_getBlockByNumber", orphan, only(id, skipParams), hist, finalized)
				require.Error(t, res.Err)
				assert.Equal(t, "reject", outcomeOf(res, id))
			})
			t.Run("by-hash lookup without the param keeps today's strict behavior", func(t *testing.T) {
				res := validateBlockVia(t, "eth_getBlockByHash", orphan, only(id, nil), hist, finalized)
				require.Error(t, res.Err)
				assert.Equal(t, "reject", outcomeOf(res, id))
			})
			t.Run("explicit byHashRequests:validate equals the default", func(t *testing.T) {
				params := map[string]string{common.IntegrityParamByHashRequests: "validate"}
				res := validateBlockVia(t, "eth_getBlockByHash", orphan, only(id, params), hist, finalized)
				require.Error(t, res.Err)
			})
			t.Run("value is case/space-insensitive", func(t *testing.T) {
				params := map[string]string{common.IntegrityParamByHashRequests: " Skip "}
				res := validateBlockVia(t, "eth_getBlockByHash", orphan, only(id, params), hist, finalized)
				assert.NoError(t, res.Err)
				assert.Equal(t, "skip", outcomeOf(res, id))
			})
		})
	}
}

// Drift guard: config validation (common) must accept exactly the
// byHashRequests vocabulary the runtime (skipsByHashLookup) normalizes —
// otherwise validation either rejects working configs or lets a
// silently-ignored value through.
func TestByHashRequestsVocabMatchesValidation(t *testing.T) {
	cfgFor := func(v string) *common.IntegrityConfig {
		return &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
			Checks: map[string]*common.IntegrityCheckConfig{
				"hashStability": {Params: map[string]string{common.IntegrityParamByHashRequests: v}},
			},
		}}
	}
	hist := mockHistory{0x11: "0xbbb"}
	orphan := blockResult("0x11", "0xddd", "0xaaa")
	for _, v := range []string{"validate", "skip", " Skip ", "VALIDATE"} {
		assert.NoError(t, cfgFor(v).Validate(), "validation must accept %q (runtime understands it)", v)
	}
	for _, v := range []string{"reject", "true", "off", "skp"} {
		assert.Error(t, cfgFor(v).Validate(), "validation must reject %q (runtime silently keeps the default)", v)
		// And confirm the runtime indeed treats it as the default (validate → strict).
		params := map[string]string{common.IntegrityParamByHashRequests: v}
		res := validateBlockVia(t, "eth_getBlockByHash", orphan, only("hashStability", params), hist, finalized)
		require.Error(t, res.Err, "unknown value %q must keep the strict default at runtime", v)
	}
}
