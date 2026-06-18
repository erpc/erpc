package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockResolver struct {
	finalized bool
	known     bool
	receipts  []Receipt
	have      bool
	fetches   *int    // optional: counts CanonicalReceipts calls (shared via pointer)
	header    *Header // optional: returned by CanonicalHeader
}

func (m mockResolver) IsFinalized(ctx context.Context, bn int64) (bool, bool) {
	return m.finalized, m.known
}
func (m mockResolver) CanonicalReceipts(ctx context.Context, ref string) ([]Receipt, bool) {
	if m.fetches != nil {
		*m.fetches++
	}
	return m.receipts, m.have
}
func (m mockResolver) CanonicalHeader(ctx context.Context, ref string) (*Header, bool) {
	return m.header, m.header != nil
}

func validateReceipt(t *testing.T, result []byte, cs CheckSet, r Resolver) Result {
	t.Helper()
	return validateReceiptPolicy(t, result, cs, r, DefaultReorgPolicy())
}

func validateReceiptPolicy(t *testing.T, result []byte, cs CheckSet, r Resolver, policy ReorgPolicy) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xaa"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getTransactionReceipt",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Resolver: r,
		Reorg:    policy,
	})
}

func TestCheck_ReceiptVsBlock(t *testing.T) {
	cs := only("receiptVsBlock", nil)
	// Narrow receipt: tx 0xaa with one log at logIndex 0x5 in block 0xbb.
	narrow := []byte(`{"blockHash":"0xbb","blockNumber":"0x10","transactionHash":"0xaa","logs":[{"logIndex":"0x5"}]}`)
	// Canonical block says that tx's log is at logIndex 0x3 → mismatch.
	mismatch := []Receipt{{BlockHash: "0xbb", TransactionHash: "0xaa", Logs: []Log{{LogIndex: "0x3"}}}}

	t.Run("finalized mismatch → reject", func(t *testing.T) {
		res := validateReceipt(t, narrow, cs, mockResolver{finalized: true, known: true, receipts: mismatch, have: true})
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("unfinalized mismatch → recorded, served", func(t *testing.T) {
		res := validateReceipt(t, narrow, cs, mockResolver{finalized: false, known: true, receipts: mismatch, have: true})
		assert.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
		assert.Equal(t, "receiptVsBlock", res.Recorded[0].CheckID)
	})

	t.Run("unknown finality → treated as unfinalized → recorded", func(t *testing.T) {
		res := validateReceipt(t, narrow, cs, mockResolver{known: false, receipts: mismatch, have: true})
		assert.NoError(t, res.Err)
		require.Len(t, res.Recorded, 1)
	})

	t.Run("matching canonical → pass", func(t *testing.T) {
		match := []Receipt{{BlockHash: "0xbb", TransactionHash: "0xaa", Logs: []Log{{LogIndex: "0x5"}}}}
		res := validateReceipt(t, narrow, cs, mockResolver{finalized: true, known: true, receipts: match, have: true})
		assert.NoError(t, res.Err)
		assert.Empty(t, res.Recorded)
	})

	t.Run("tx missing from canonical block → reject when finalized", func(t *testing.T) {
		other := []Receipt{{BlockHash: "0xbb", TransactionHash: "0xff", Logs: []Log{{LogIndex: "0x0"}}}}
		res := validateReceipt(t, narrow, cs, mockResolver{finalized: true, known: true, receipts: other, have: true})
		require.Error(t, res.Err)
	})

	t.Run("resolver cannot fetch → no-op", func(t *testing.T) {
		res := validateReceipt(t, narrow, cs, mockResolver{finalized: true, known: true, have: false})
		assert.NoError(t, res.Err)
	})

	t.Run("no resolver → no-op", func(t *testing.T) {
		res := validateReceipt(t, narrow, cs, nil)
		assert.NoError(t, res.Err)
	})

	t.Run("unfinalized:off skips the force-fetch entirely", func(t *testing.T) {
		fetches := 0
		policy := ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorIgnore}
		res := validateReceiptPolicy(t, narrow, cs,
			mockResolver{known: true, finalized: false, receipts: mismatch, have: true, fetches: &fetches}, policy)
		assert.NoError(t, res.Err)
		assert.Empty(t, res.Recorded)
		assert.Equal(t, 0, fetches, "an ignored reorg-sensitive check must issue no canonical fetch")
	})
}
