package integrity

// Realistic signed full blocks, so every recompute check runs to completion
// (fake r/s values fail geth's signature sanity check and skip the recompute
// early, which would hide both the cost and the decode paths under test).

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"github.com/erpc/erpc/architecture/evm/integrity/internal/legacy"
	"github.com/erpc/erpc/common"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	benchBlockMu    sync.Mutex
	benchBlockCache = map[int][]byte{}
)

// realisticSignedBlock returns an eth_getBlockByNumber(full=true) result with
// n signed transactions (mix: 70% EIP-1559 ERC20 transfers, 15% EIP-1559 with
// ~1KB calldata + access list, 15% legacy EIP-155), a correct transactionsRoot
// and a correct block hash.
func realisticSignedBlock(tb testing.TB, num int64, n int) []byte {
	benchBlockMu.Lock()
	defer benchBlockMu.Unlock()
	if b, ok := benchBlockCache[n]; ok {
		return b
	}
	chainID := big.NewInt(1)
	signer := gethtypes.LatestSignerForChainID(chainID)
	keys := make([]*ecdsa.PrivateKey, 16)
	for i := range keys {
		k, err := crypto.GenerateKey()
		if err != nil {
			tb.Fatal(err)
		}
		keys[i] = k
	}
	txs := make(gethtypes.Transactions, 0, n)
	for i := 0; i < n; i++ {
		to := gethcommon.BigToAddress(big.NewInt(int64(0x1000 + i)))
		var data []byte
		var inner gethtypes.TxData
		switch {
		case i%20 < 14:
			data = append(hexutil.MustDecode("0xa9059cbb"), gethcommon.LeftPadBytes(to.Bytes(), 32)...)
			data = append(data, gethcommon.LeftPadBytes(big.NewInt(int64(i+1)*1e15).Bytes(), 32)...)
			inner = &gethtypes.DynamicFeeTx{ChainID: chainID, Nonce: uint64(i), GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 65000, To: &to, Value: big.NewInt(0), Data: data}
		case i%20 < 17:
			data = make([]byte, 1028)
			for j := range data {
				data[j] = byte(i*31 + j)
			}
			al := gethtypes.AccessList{{Address: to, StorageKeys: []gethcommon.Hash{gethcommon.BigToHash(big.NewInt(int64(i))), gethcommon.BigToHash(big.NewInt(int64(i + 1)))}}}
			inner = &gethtypes.DynamicFeeTx{ChainID: chainID, Nonce: uint64(i), GasTipCap: big.NewInt(2e9), GasFeeCap: big.NewInt(40e9), Gas: 350000, To: &to, Value: big.NewInt(int64(i) * 1e12), Data: data, AccessList: al}
		default:
			inner = &gethtypes.LegacyTx{Nonce: uint64(i), GasPrice: big.NewInt(25e9), Gas: 21000, To: &to, Value: new(big.Int).Mul(big.NewInt(int64(i+1)), big.NewInt(1e16))}
		}
		tx, err := gethtypes.SignNewTx(keys[i%len(keys)], signer, inner)
		if err != nil {
			tb.Fatal(err)
		}
		txs = append(txs, tx)
	}
	h := &gethtypes.Header{
		ParentHash: gethcommon.HexToHash("0x11"), UncleHash: gethtypes.EmptyUncleHash,
		Coinbase: gethcommon.HexToAddress("0x95222290dd7278aa3ddd389cc1e1d165cc4bafe5"),
		Root:     gethcommon.HexToHash("0x22"), TxHash: gethtypes.DeriveSha(txs, trie.NewStackTrie(nil)),
		ReceiptHash: gethcommon.HexToHash("0x33"), Difficulty: big.NewInt(0), Number: big.NewInt(num),
		GasLimit: 30_000_000, GasUsed: 20_000_000, Time: 1_700_000_000, Extra: []byte("bench"),
		BaseFee: big.NewInt(9e8), WithdrawalsHash: &gethtypes.EmptyWithdrawalsHash,
	}
	var bloom gethtypes.Bloom
	for i := 0; i < 256; i += 3 {
		bloom[i] = byte(i)
	}
	h.Bloom = bloom
	blockHash := h.Hash()

	hraw, err := h.MarshalJSON()
	if err != nil {
		tb.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(hraw, &m); err != nil {
		tb.Fatal(err)
	}
	txObjs := make([]json.RawMessage, 0, n)
	for i, tx := range txs {
		traw, err := tx.MarshalJSON()
		if err != nil {
			tb.Fatal(err)
		}
		var tm map[string]json.RawMessage
		_ = json.Unmarshal(traw, &tm)
		from, _ := gethtypes.Sender(signer, tx)
		set := func(k string, v any) { b, _ := json.Marshal(v); tm[k] = b }
		set("blockHash", blockHash.Hex())
		set("blockNumber", fmt.Sprintf("0x%x", num))
		set("from", from.Hex())
		set("transactionIndex", fmt.Sprintf("0x%x", i))
		if tx.Type() != gethtypes.LegacyTxType {
			set("gasPrice", "0x6fc23ac00")
		}
		b, _ := json.Marshal(tm)
		txObjs = append(txObjs, b)
	}
	tb2, _ := json.Marshal(txObjs)
	m["transactions"] = tb2
	m["uncles"] = json.RawMessage(`[]`)
	m["withdrawals"] = json.RawMessage(`[]`)
	m["size"] = json.RawMessage(`"0x2a1f3"`)
	out, err := json.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	benchBlockCache[n] = out
	return out
}

// BenchmarkValidateFullBlock: full Validate (level intrinsic, chain 1) on a
// signed block whose every check runs to "pass", current implementation vs
// the frozen pre-shared-decode copy in internal/legacy.
func BenchmarkValidateFullBlock(b *testing.B) {
	for _, n := range []int{150, 1000} {
		raw := realisticSignedBlock(b, 0x1234, n)
		r := blockReq("eth_getBlockByNumber", "0x1234", true)
		for _, impl := range []struct {
			name string
			run  func(testing.TB, parityReq, []byte) verdict
		}{{"legacy", runLegacy}, {"new", runNew}} {
			b.Run(fmt.Sprintf("txs=%d/%s", n, impl.name), func(b *testing.B) {
				v := impl.run(b, r, raw)
				if v.Err || outcomeIn(v, "transactionsRootRecompute") != "pass" || outcomeIn(v, "blockHashRecompute") != "pass" {
					b.Fatalf("recompute did not run to completion: %s", v)
				}
				in := benchInputFor(b, impl.name, raw)
				b.SetBytes(int64(len(raw)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					in()
				}
			})
		}
	}
}

// benchInputFor builds the request/response once and returns a closure that
// runs one Validate, so the loop measures validation only.
func benchInputFor(b *testing.B, impl string, raw []byte) func() {
	r := blockReq("eth_getBlockByNumber", "0x1234", true)
	rs := parityResponse(b, r, raw)
	if impl == "legacy" {
		cs := legacy.CheckSetForLevel(legacy.LevelIntrinsic)
		legacy.ApplyChainProfile(cs, 1)
		in := legacy.Input{Reorg: legacy.DefaultReorgPolicy(), Method: r.method, Upstream: common.NewFakeUpstream("u"), Response: rs, Checks: cs, Params: r.params}
		return func() { _ = legacy.Validate(context.Background(), in) }
	}
	cs := CheckSetForLevel(LevelIntrinsic)
	ApplyChainProfile(cs, 1)
	in := Input{Reorg: DefaultReorgPolicy(), Method: r.method, Upstream: common.NewFakeUpstream("u"), Response: rs, Checks: cs, Params: r.params}
	return func() { _ = Validate(context.Background(), in) }
}
