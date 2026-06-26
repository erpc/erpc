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
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	in := Input{
		Method:   "eth_getBlockByNumber",
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
