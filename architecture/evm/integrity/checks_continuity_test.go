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

// Continuity is a by-NUMBER question ("what is the chain at height N"). An
// explicit by-hash lookup names the block it wants and may legitimately be an
// orphan (reorg unwinding), so the continuity pair does not apply there at all
// — not registered, no outcome, nothing to configure.
func TestCheck_Continuity_ByHashLookupsAreNotSubjectToContinuity(t *testing.T) {
	hist := mockHistory{0x10: "0xaaa", 0x11: "0xbbb"} // pins: 16→0xaaa, 17→0xbbb
	// An orphan: both checks would fire on it — hash ≠ pin(17), parent ≠ pin(16).
	orphan := blockResult("0x11", "0xddd", "0xccc")

	for _, id := range []string{"hashStability", "parentHashLinkage"} {
		t.Run(id, func(t *testing.T) {
			t.Run("by-hash lookup of an orphan is served untouched", func(t *testing.T) {
				res := validateBlockVia(t, "eth_getBlockByHash", orphan, only(id, nil), hist, finalized)
				assert.NoError(t, res.Err)
				assert.Empty(t, res.Recorded)
				assert.Empty(t, outcomeOf(res, id), "the check must not run for eth_getBlockByHash")
			})
			t.Run("by-number lookup of the same body still rejects", func(t *testing.T) {
				res := validateBlockVia(t, "eth_getBlockByNumber", orphan, only(id, nil), hist, finalized)
				require.Error(t, res.Err)
				assert.Equal(t, "reject", outcomeOf(res, id))
			})
			t.Run("the check is registered for by-number only", func(t *testing.T) {
				assert.False(t, hasCheckFor(MethodGetBlockByHash, id))
				assert.True(t, hasCheckFor(MethodGetBlockByNumber, id))
			})
		})
	}
}

func hasCheckFor(method, id string) bool {
	for _, c := range checksFor(method) {
		if c.ID == id {
			return true
		}
	}
	return false
}
