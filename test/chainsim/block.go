// Package chainsim simulates one EVM chain and the vendor nodes that serve
// it over real sockets: HTTP JSON-RPC and WebSocket eth_subscribe, each
// vendor with its own view of the chain and its own app-level faults.
package chainsim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

// Hash is a block, transaction or topic hash.
type Hash = gethcommon.Hash

// Logs of simulated blocks use these addresses and topic0s. Filters match
// the first address and the second topic0.
var (
	Addrs = []gethcommon.Address{
		gethcommon.HexToAddress("0x00000000000000000000000000000000000000a1"),
		gethcommon.HexToAddress("0x00000000000000000000000000000000000000a2"),
		gethcommon.HexToAddress("0x00000000000000000000000000000000000000a3"),
		gethcommon.HexToAddress("0x00000000000000000000000000000000000000a4"),
	}
	Topics = []Hash{
		gethcommon.HexToHash("0x01"),
		gethcommon.HexToHash("0x02"),
		gethcommon.HexToHash("0x03"),
	}
)

// Filter is a logs subscription filter: one address, or one topic0.
type Filter struct {
	Address *gethcommon.Address
	Topic0  *Hash
}

// Filters are the two filters logs clients subscribe; a matching block
// holds a log of each, a quiet block none.
var Filters = []Filter{
	{Address: &Addrs[0]},
	{Topic0: &Topics[1]},
}

// Params is the filter as eth_subscribe and eth_getLogs take it.
func (f Filter) Params() map[string]interface{} {
	if f.Address != nil {
		return map[string]interface{}{"address": f.Address.Hex()}
	}
	return map[string]interface{}{"topics": []interface{}{f.Topic0.Hex()}}
}

// Matches reports whether the filter selects l.
func (f Filter) Matches(l Log) bool {
	if f.Address != nil {
		return l.Address == *f.Address
	}
	return len(l.Topics) > 0 && l.Topics[0] == *f.Topic0
}

// Log is one log of a simulated block.
type Log struct {
	BlockHash   Hash
	BlockNumber uint64
	TxHash      Hash
	TxIndex     int
	LogIndex    int
	Address     gethcommon.Address
	Topics      []Hash
}

// LogKey identifies a log across answers.
type LogKey struct {
	BlockHash Hash
	LogIndex  int
}

func (l Log) Key() LogKey { return LogKey{l.BlockHash, l.LogIndex} }

func (l Log) json() map[string]interface{} {
	topics := make([]string, len(l.Topics))
	for i, t := range l.Topics {
		topics[i] = t.Hex()
	}
	return map[string]interface{}{
		"address": strings.ToLower(l.Address.Hex()), "topics": topics, "data": "0x",
		"blockHash": l.BlockHash.Hex(), "blockNumber": hexU(l.BlockNumber),
		"transactionHash": l.TxHash.Hex(), "transactionIndex": hexU(uint64(l.TxIndex)),
		"logIndex": hexU(uint64(l.LogIndex)), "removed": false,
	}
}

// Kind is what a block holds: no transactions, logs neither filter
// matches, or logs both filters match.
type Kind uint8

const (
	KindEmpty Kind = iota
	KindQuiet
	KindMatching
)

type logSpec struct{ addr, topic int }

// Block is one minted block; immutable once minted.
type Block struct {
	Number   uint64
	Hash     Hash
	Parent   *Block // nil for the first history block
	Time     uint64
	MintedAt time.Time // when it became a vendor's view: the start of its delivery latency
	minority bool
	kind     Kind
	tag      string
	txs      [][]logSpec
	raw      json.RawMessage
	// logs are computed once (Logs): a client checks a big block's every
	// log against them.
	logsOnce sync.Once
	logs     []Log
}

// ParentHash is the header's parentHash.
func (b *Block) ParentHash() Hash {
	if b.Parent == nil {
		return Hash{}
	}
	return b.Parent.Hash
}

// Minority reports whether the block was minted on a minority fork.
func (b *Block) Minority() bool { return b.minority }

// Kind is what the block holds.
func (b *Block) Kind() Kind { return b.kind }

// TxCount is how many transactions the block holds.
func (b *Block) TxCount() int { return len(b.txs) }

// TxHash is the hash of the block's transaction i.
func (b *Block) TxHash(i int) Hash { return txHash(b.Hash, i) }

// Receipts are the block's receipts as an honest vendor answers them.
func (b *Block) Receipts() []map[string]interface{} { return b.receipts(false, false, 0, false) }

// Receipt is transaction i's receipt as an honest vendor answers it.
func (b *Block) Receipt(i int) map[string]interface{} {
	li := 0
	for _, tx := range b.txs[:i] {
		li += len(tx)
	}
	return b.receiptAt(i, li, false, false, false)
}

// JSON is the header as eth_getBlockByNumber(n, false) answers it.
func (b *Block) JSON() json.RawMessage { return b.raw }

// Logs are the block's logs, in logIndex order; callers must not modify
// them.
func (b *Block) Logs() []Log {
	b.logsOnce.Do(func() {
		i := 0
		for tx, logs := range b.txs {
			for _, l := range logs {
				b.logs = append(b.logs, Log{
					BlockHash: b.Hash, BlockNumber: b.Number, TxHash: txHash(b.Hash, tx), TxIndex: tx, LogIndex: i,
					Address: Addrs[l.addr], Topics: []Hash{Topics[l.topic], txTopic(b.tag, tx)},
				})
				i++
			}
		}
	})
	return b.logs
}

// receipts are the block's receipts as a vendor answers them: badBloom
// zeroes each receipt's logsBloom, offBloom adds a bit the block's logs do
// not set, drop leaves out the last receipts, forge changes the first
// log's data.
func (b *Block) receipts(badBloom, offBloom bool, drop int, forge bool) []map[string]interface{} {
	n := max(0, len(b.txs)-drop)
	out := make([]map[string]interface{}, 0, n)
	li := 0
	for i := range n {
		out = append(out, b.receiptAt(i, li, badBloom, offBloom, forge))
		li += len(b.txs[i])
	}
	return out
}

// receiptAt is transaction i's receipt, whose first log is the block's log
// li, with receipts' faults.
func (b *Block) receiptAt(i, li int, badBloom, offBloom, forge bool) map[string]interface{} {
	logs := b.Logs()
	var bloom gethtypes.Bloom
	if !badBloom {
		addTxBloom(&bloom, b.tag, i, b.txs[i])
	}
	if offBloom {
		bloom.Add([]byte("off-bloom"))
	}
	rl := make([]map[string]interface{}, 0, len(b.txs[i]))
	for k := range b.txs[i] {
		j := logs[li+k].json()
		if forge && li+k == 0 {
			j["data"] = ForgedData
		}
		rl = append(rl, j)
	}
	return map[string]interface{}{
		"blockHash": b.Hash.Hex(), "blockNumber": hexU(b.Number),
		"transactionHash": txHash(b.Hash, i).Hex(), "transactionIndex": hexU(uint64(i)),
		"status": "0x1", "cumulativeGasUsed": "0x5208", "gasUsed": "0x5208", "type": "0x0",
		"logsBloom": "0x" + hex.EncodeToString(bloom[:]), "logs": rl,
	}
}

// lastLogCovered reports whether the block's other logs carry its last
// log's address, topic0 and transaction topic, so the bloom of the logs
// without it is the header's.
func (b *Block) lastLogCovered() bool {
	if len(b.txs) == 0 {
		return false
	}
	lastTx := b.txs[len(b.txs)-1]
	if len(lastTx) < 2 {
		return false
	}
	last := lastTx[len(lastTx)-1]
	addr, topic := false, false
	for i, tx := range b.txs {
		for k, l := range tx {
			if i == len(b.txs)-1 && k == len(tx)-1 {
				continue
			}
			addr, topic = addr || l.addr == last.addr, topic || l.topic == last.topic
		}
	}
	return addr && topic
}

// ForgedData is the data of a forged log: no block of any chain holds it.
const ForgedData = "0xbad0"

// content is a block's transactions and their logs; txs > 0 sets how many
// transactions a non-empty block holds.
func content(r *rand.Rand, k Kind, txs int) [][]logSpec {
	if k == KindEmpty {
		return nil
	}
	if txs == 0 {
		txs = 12 + r.IntN(5)
	}
	out := make([][]logSpec, txs)
	for i := range out {
		for range 1 + r.IntN(2) {
			l := logSpec{addr: r.IntN(len(Addrs)), topic: r.IntN(len(Topics))}
			if k == KindQuiet {
				l = logSpec{addr: 2 + r.IntN(2), topic: []int{0, 2}[r.IntN(2)]}
			}
			out[i] = append(out[i], l)
		}
	}
	if k == KindMatching {
		out[0][0] = logSpec{addr: 0, topic: 1}
	}
	return out
}

// addTxBloom adds a transaction's logs to bloom. Every log carries a second
// topic unique to its transaction, so receipts that miss a transaction
// never match the header's bloom.
func addTxBloom(bloom *gethtypes.Bloom, tag string, tx int, logs []logSpec) {
	own := txTopic(tag, tx)
	for _, l := range logs {
		bloom.Add(Addrs[l.addr].Bytes())
		bloom.Add(Topics[l.topic].Bytes())
		bloom.Add(own.Bytes())
	}
}

func txTopic(tag string, tx int) Hash {
	return sha256.Sum256(fmt.Appendf(nil, "topic/%s/%d", tag, tx))
}

func txHash(block Hash, i int) Hash {
	return sha256.Sum256(fmt.Appendf(nil, "%x/%d", block, i))
}

func receiptsRoot(tag string, txs [][]logSpec) Hash {
	rs := make(gethtypes.Receipts, len(txs))
	for i, tx := range txs {
		var bloom gethtypes.Bloom
		addTxBloom(&bloom, tag, i, tx)
		logs := make([]*gethtypes.Log, len(tx))
		for k, l := range tx {
			logs[k] = &gethtypes.Log{Address: Addrs[l.addr], Topics: []Hash{Topics[l.topic], txTopic(tag, i)}, Data: []byte{}}
		}
		rs[i] = &gethtypes.Receipt{Type: gethtypes.LegacyTxType, Status: gethtypes.ReceiptStatusSuccessful, CumulativeGasUsed: 0x5208, Bloom: bloom, Logs: logs}
	}
	return gethtypes.DeriveSha(rs, trie.NewStackTrie(nil))
}

// header builds a block whose header hashes to its hash (blockHashRecompute)
// and whose logsBloom is the OR of its receipts' blooms, or with offBloom
// and transactions that OR with one more value added (OffHeaderBloom). A
// foreign header (Config.Foreign) carries a hash it does not hash to, and
// an empty one the roots keccak("") instead of the empty-trie root.
func header(chainID int64, parent *Block, number uint64, ts uint64, tag string, txs [][]logSpec, pad int, baseFee int64, offBloom, foreign bool) (Hash, json.RawMessage) {
	var ph Hash
	if parent != nil {
		ph = parent.Hash
	}
	var bloom gethtypes.Bloom
	for i, tx := range txs {
		addTxBloom(&bloom, tag, i, tx)
	}
	if offBloom && len(txs) > 0 {
		bloom.Add([]byte("chainsim/header-bloom-off"))
	}
	hdr := &gethtypes.Header{
		ParentHash:  ph,
		UncleHash:   gethtypes.EmptyUncleHash,
		Coinbase:    gethcommon.HexToAddress("0x0000000000000000000000000000000000000001"),
		TxHash:      gethtypes.EmptyTxsHash,
		ReceiptHash: gethtypes.EmptyReceiptsHash,
		Bloom:       bloom,
		Difficulty:  big.NewInt(0),
		Number:      new(big.Int).SetUint64(number),
		GasLimit:    30_000_000,
		Time:        ts,
		Extra:       append(fmt.Appendf(nil, "%d/%s", chainID, tag), make([]byte, pad)...),
		BaseFee:     big.NewInt(baseFee),
	}
	if len(txs) > 0 {
		hdr.TxHash = sha256.Sum256(fmt.Appendf(nil, "txs/%d/%s", chainID, tag))
		hdr.ReceiptHash = receiptsRoot(tag, txs)
		hdr.GasUsed = uint64(len(txs)) * 21000
	} else if foreign {
		hdr.TxHash, hdr.ReceiptHash = gethcrypto.Keccak256Hash(), gethcrypto.Keccak256Hash()
	}
	hash := hdr.Hash()
	if foreign {
		hash = sha256.Sum256(append([]byte("foreign/"), hash[:]...))
	}
	raw, err := hdr.MarshalJSON()
	if err != nil {
		panic(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	fields["size"] = json.RawMessage(`"0x220"`)
	fields["hash"], _ = json.Marshal(hash.Hex())
	hashes := make([]string, len(txs))
	for i := range txs {
		hashes[i] = txHash(hash, i).Hex()
	}
	fields["transactions"], _ = json.Marshal(hashes)
	fields["uncles"] = json.RawMessage(`[]`)
	raw, _ = json.Marshal(fields)
	return hash, raw
}

// ForgeHash is raw with a hash it does not hash to.
func ForgeHash(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	h := sha256.Sum256(fields["hash"])
	fields["hash"], _ = json.Marshal("0x" + hex.EncodeToString(h[:]))
	out, _ := json.Marshal(fields)
	return out
}

// WithNumber is raw with its number field set to n: a header that claims
// a height it is not at.
func WithNumber(raw json.RawMessage, n uint64) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic(err)
	}
	fields["number"], _ = json.Marshal(hexU(n))
	out, _ := json.Marshal(fields)
	return out
}

// ForgedHashOf is the hash a forged push of b carries.
func ForgedHashOf(b *Block) Hash {
	q, _ := json.Marshal(b.Hash.Hex())
	return sha256.Sum256(q)
}

func hexU(n uint64) string { return fmt.Sprintf("0x%x", n) }

// pendingTx is transaction h as a full newPendingTransactions push sends
// it: a legacy transfer with no block.
func pendingTx(chainID int64, h Hash) map[string]interface{} {
	return map[string]interface{}{
		"hash": h.Hex(), "type": "0x0", "nonce": "0x1", "from": Addrs[1].Hex(), "to": Addrs[2].Hex(),
		"value": "0x1", "gas": "0x5208", "gasPrice": "0x7", "input": "0x", "chainId": hexU(uint64(chainID)),
		"v": "0x1b", "r": "0x01", "s": "0x02",
	}
}
