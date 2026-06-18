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

// signedTxJSON signs a real EIP-1559 transaction and renders it the way an RPC
// would (the signed fields plus the RPC-added "from"). go-ethereum is the oracle.
func signedTxJSON(t *testing.T) (raw []byte, from, hash string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	chainID := big.NewInt(1)
	to := gethcommon.HexToAddress("0x00000000000000000000000000000000000000aa")
	tx, err := gethtypes.SignNewTx(key, gethtypes.LatestSignerForChainID(chainID), &gethtypes.DynamicFeeTx{
		ChainID: chainID, Nonce: 7, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(100),
		Gas: 21000, To: &to, Value: big.NewInt(1000),
	})
	require.NoError(t, err)
	from = crypto.PubkeyToAddress(key.PublicKey).Hex()

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
}
