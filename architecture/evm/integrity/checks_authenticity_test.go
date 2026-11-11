package integrity

import (
	"encoding/json"
	"math/big"
	"testing"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signTx signs a real EIP-1559 transaction with a fresh key. go-ethereum is the
// oracle for every authenticity/commitment test.
func signTx(t *testing.T, nonce uint64) (*gethtypes.Transaction, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	chainID := big.NewInt(1)
	to := gethcommon.HexToAddress("0x00000000000000000000000000000000000000aa")
	tx, err := gethtypes.SignNewTx(key, gethtypes.LatestSignerForChainID(chainID), &gethtypes.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(100),
		Gas: 21000, To: &to, Value: big.NewInt(1000),
	})
	require.NoError(t, err)
	return tx, crypto.PubkeyToAddress(key.PublicKey).Hex()
}

// signedTxJSON renders a signed tx the way an RPC would (signed fields plus the
// RPC-added "from").
func signedTxJSON(t *testing.T) (raw []byte, from, hash string) {
	t.Helper()
	tx, from := signTx(t, 7)
	b, err := tx.MarshalJSON()
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	fb, _ := json.Marshal(from)
	m["from"] = fb
	raw, _ = json.Marshal(m)
	return raw, from, tx.Hash().Hex()
}

func mutateField(t *testing.T, raw []byte, key, val string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	vb, _ := json.Marshal(val)
	m[key] = vb
	out, _ := json.Marshal(m)
	return out
}

func TestCheck_SenderRecovery(t *testing.T) {
	raw, _, _ := signedTxJSON(t)
	cs := only("senderRecovery", nil)

	t.Run("valid signature recovers the claimed sender", func(t *testing.T) {
		assert.NoError(t, run(t, MethodGetTransactionByHash, raw, cs))
	})

	t.Run("wrong from is rejected", func(t *testing.T) {
		bad := mutateField(t, raw, "from", "0x0000000000000000000000000000000000000001")
		require.Error(t, run(t, MethodGetTransactionByHash, bad, cs))
	})

	t.Run("corrupted signed field → hash gate skips (no false positive)", func(t *testing.T) {
		// Alter a signed field but keep the claimed hash: the tx no longer hashes
		// to the claimed value, so the decoder is not trusted → skip, not reject.
		skip := mutateField(t, raw, "value", "0x270f")
		assert.NoError(t, run(t, MethodGetTransactionByHash, skip, cs))
	})

	t.Run("unsigned tx (no signature) is skipped", func(t *testing.T) {
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &m))
		delete(m, "r")
		nosig, _ := json.Marshal(m)
		assert.NoError(t, run(t, MethodGetTransactionByHash, nosig, cs))
	})

	// Real mainnet pre-EIP-155 legacy tx (v=27, no chain id). geth's chain-id
	// signers panic on it; this used to crash the process.
	legacy := []byte(`{"blockHash":"0xdaa690a16bc4a2bf2ff8955ff1cf3bc2155bc9c53f5731b6f8706fdc2b33b82c","blockNumber":"0xa2d2e3","from":"0x3fab184622dc19b6109349b94811493bf2a45362","gas":"0x186a0","gasPrice":"0x174876e800","hash":"0xeddf9e61fb9d8f5111840daef55e5fde0041f5702856532cdbb5a02998033d26","input":"0x604580600e600039806000f350fe7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3","nonce":"0x0","to":null,"transactionIndex":"0x2a","value":"0x0","type":"0x0","v":"0x1b","r":"0x2222222222222222222222222222222222222222222222222222222222222222","s":"0x2222222222222222222222222222222222222222222222222222222222222222"}`)

	t.Run("pre-EIP-155 tx recovers its sender", func(t *testing.T) {
		res := runRes(t, MethodGetTransactionByHash, legacy, cs)
		require.NoError(t, res.Err)
		assert.Equal(t, []CheckOutcome{{"senderRecovery", "pass"}}, res.Outcomes)
	})

	t.Run("pre-EIP-155 tx with wrong from is rejected", func(t *testing.T) {
		bad := mutateField(t, legacy, "from", "0x0000000000000000000000000000000000000001")
		require.Error(t, run(t, MethodGetTransactionByHash, bad, cs))
	})
}
