package integrity

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func blockReceipts(receipts ...string) []byte {
	return []byte("[" + strings.Join(receipts, ",") + "]")
}

func assertRejected(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointContentValidation),
		"expected ErrEndpointContentValidation, got %v", err)
}

func only(id string, params map[string]string) CheckSet { return CheckSet{}.Enable(id, params) }

func TestCheck_TxHashUniqueness(t *testing.T) {
	dup := blockReceipts(
		`{"transactionHash":"0xabc","transactionIndex":"0x0","logs":[]}`,
		`{"transactionHash":"0xABC","transactionIndex":"0x1","logs":[]}`, // same, different case
	)
	assertRejected(t, run(t, MethodGetBlockReceipts, dup, only("txHashUniqueness", nil)))

	unique := blockReceipts(
		`{"transactionHash":"0xabc","logs":[]}`,
		`{"transactionHash":"0xdef","logs":[]}`,
	)
	assert.NoError(t, run(t, MethodGetBlockReceipts, unique, only("txHashUniqueness", nil)))

	// strict rejects an empty hash; non-strict tolerates it.
	emptyHash := blockReceipts(`{"transactionHash":"","logs":[]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, emptyHash, only("txHashUniqueness", nil)))
	assertRejected(t, run(t, MethodGetBlockReceipts, emptyHash, only("txHashUniqueness", map[string]string{"strict": "true"})))
}

func TestCheck_SameBlockHash(t *testing.T) {
	cs := only("sameBlockHash", nil)
	assert.NoError(t, run(t, MethodGetBlockReceipts, blockReceipts(
		`{"blockHash":"0xaa","logs":[]}`, `{"blockHash":"0xAA","logs":[]}`,
	), cs))
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(
		`{"blockHash":"0xaa","logs":[]}`, `{"blockHash":"0xbb","logs":[]}`,
	), cs))
}

func TestCheck_TransactionIndexConsistency(t *testing.T) {
	cs := only("transactionIndexConsistency", nil)
	assert.NoError(t, run(t, MethodGetBlockReceipts, blockReceipts(
		`{"transactionIndex":"0x0","logs":[]}`, `{"transactionIndex":"0x1","logs":[]}`,
	), cs))
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(
		`{"transactionIndex":"0x0","logs":[]}`, `{"transactionIndex":"0x5","logs":[]}`,
	), cs))
}

func TestCheck_LogFieldShapes(t *testing.T) {
	cs := only("logFieldShapes", nil)
	addr := "0x27b26e88f007ec9109648c6da522fcaba06c74d7" // 20 bytes
	topic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	good := blockReceipts(`{"logs":[{"address":"` + addr + `","topics":["` + topic + `"]}]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, good, cs))

	shortAddr := blockReceipts(`{"logs":[{"address":"0x1234","topics":[]}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, shortAddr, cs))

	shortTopic := blockReceipts(`{"logs":[{"address":"` + addr + `","topics":["0xabcd"]}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, shortTopic, cs))

	tooMany := blockReceipts(`{"logs":[{"address":"` + addr + `","topics":["` + topic + `","` + topic + `","` + topic + `","` + topic + `","` + topic + `"]}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, tooMany, cs))
}

func TestCheck_LogMetadata(t *testing.T) {
	cs := only("logMetadata", nil)
	good := blockReceipts(`{"blockHash":"0xaa","blockNumber":"0x10","transactionHash":"0x1","transactionIndex":"0x0","logs":[{"blockHash":"0xAA","blockNumber":"0x10","transactionHash":"0x1","transactionIndex":"0x00"}]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, good, cs))

	// log blockHash differs from its parent receipt — the mixed-block bug.
	mixed := blockReceipts(`{"blockHash":"0xaa","transactionHash":"0x1","logs":[{"blockHash":"0xbb","transactionHash":"0x1"}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, mixed, cs))

	wrongTx := blockReceipts(`{"blockHash":"0xaa","transactionHash":"0x1","logs":[{"blockHash":"0xaa","transactionHash":"0x9"}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, wrongTx, cs))
}

func TestCheck_BloomEmptiness(t *testing.T) {
	cs := only("bloomEmptiness", nil)
	zero := "0x" + strings.Repeat("0", 512)
	// zero bloom + no logs: ok
	assert.NoError(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"logsBloom":"`+zero+`","logs":[]}`), cs))
	// zero bloom + logs: rejected
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"logsBloom":"`+zero+`","logs":[{"address":"0x27b26e88f007ec9109648c6da522fcaba06c74d7"}]}`), cs))
	// non-zero bloom + no logs: rejected
	nonZero := "0x" + strings.Repeat("0", 510) + "01"
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"logsBloom":"`+nonZero+`","logs":[]}`), cs))
}

func TestCheck_BloomMatch(t *testing.T) {
	cs := only("bloomMatch", nil)
	addr := "0x27b26e88f007ec9109648c6da522fcaba06c74d7"
	topic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	logs := []Log{{Address: addr, Topics: []string{topic}}}
	computed, err := bloomFromLogs(logs)
	require.NoError(t, err)
	correct := "0x" + hex.EncodeToString(computed)

	good := blockReceipts(`{"logsBloom":"` + correct + `","logs":[{"address":"` + addr + `","topics":["` + topic + `"]}]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, good, cs), "a receipt with a correctly-recomputed bloom must pass")

	wrong := "0x" + strings.Repeat("0", 512)
	bad := blockReceipts(`{"logsBloom":"` + wrong + `","logs":[{"address":"` + addr + `","topics":["` + topic + `"]}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, bad, cs))
}

func TestCheck_LogIndexContiguity(t *testing.T) {
	cs := only("logIndexContiguity", nil)
	good := blockReceipts(`{"logs":[{"logIndex":"0x0"},{"logIndex":"0x1"}]}`, `{"logs":[{"logIndex":"0x2"}]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, good, cs))

	gap := blockReceipts(`{"logs":[{"logIndex":"0x0"},{"logIndex":"0x2"}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, gap, cs))

	missing := blockReceipts(`{"logs":[{"logIndex":""}]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, missing, cs))
}

func TestCheck_ExpectedBlock(t *testing.T) {
	cs := only("expectedBlock", map[string]string{"hash": "0xaa", "number": "16"})
	assert.NoError(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"blockHash":"0xAA","blockNumber":"0x10","logs":[]}`), cs))
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"blockHash":"0xbb","blockNumber":"0x10","logs":[]}`), cs))
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"blockHash":"0xaa","blockNumber":"0x11","logs":[]}`), cs))
}

func TestCheck_ReceiptsCount(t *testing.T) {
	three := blockReceipts(`{"logs":[]}`, `{"logs":[]}`, `{"logs":[]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, three, only("receiptsCount", map[string]string{"exact": "3"})))
	assertRejected(t, run(t, MethodGetBlockReceipts, three, only("receiptsCount", map[string]string{"exact": "2"})))
	assert.NoError(t, run(t, MethodGetBlockReceipts, three, only("receiptsCount", map[string]string{"atLeast": "3"})))
	assertRejected(t, run(t, MethodGetBlockReceipts, three, only("receiptsCount", map[string]string{"atLeast": "4"})))
}

func TestCheck_ReceiptTransactionMatch(t *testing.T) {
	h0, _ := common.HexToBytes("0x1111")
	h1, _ := common.HexToBytes("0x2222")
	gt := []GroundTruthTx{{Hash: h0}, {Hash: h1}}
	cfg := CheckConfig{Enabled: true, Data: gt}
	cs := CheckSet{"receiptTransactionMatch": cfg}

	good := blockReceipts(`{"transactionHash":"0x1111","logs":[]}`, `{"transactionHash":"0x2222","logs":[]}`)
	assert.NoError(t, run(t, MethodGetBlockReceipts, good, cs))

	wrongHash := blockReceipts(`{"transactionHash":"0x1111","logs":[]}`, `{"transactionHash":"0x9999","logs":[]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, wrongHash, cs))

	wrongCount := blockReceipts(`{"transactionHash":"0x1111","logs":[]}`)
	assertRejected(t, run(t, MethodGetBlockReceipts, wrongCount, cs))

	// contract-creation consistency: tx with no `To` must yield a contractAddress.
	creation := []GroundTruthTx{{Hash: h0, To: nil}}
	csCreate := CheckSet{"receiptTransactionMatch": CheckConfig{Enabled: true, Params: map[string]string{"contractCreation": "true"}, Data: creation}}
	assertRejected(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"transactionHash":"0x1111","contractAddress":"","logs":[]}`), csCreate))
	assert.NoError(t, run(t, MethodGetBlockReceipts, blockReceipts(`{"transactionHash":"0x1111","contractAddress":"0x27b26e88f007ec9109648c6da522fcaba06c74d7","logs":[]}`), csCreate))
}
