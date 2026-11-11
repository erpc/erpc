package integrity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Header formats whose hash is not a plain RLP field list.

// tempoHeaderLayout is the alloy header Tempo nests inside its own.
var tempoHeaderLayout = rlpLayout{
	required: gethBaseFields,
	optional: []headerField{
		quantityField("baseFeePerGas"), bytesField("withdrawalsRoot"), quantityField("blobGasUsed"),
		quantityField("excessBlobGas"), bytesField("parentBeaconBlockRoot"), bytesField("requestsHash"),
	},
}

// tempoBlockHash: keccak256(RLP([mainBlockGeneralGasLimit, sharedGasLimit,
// timestampMillisPart, header, consensusContext?])), the context last and
// optional (tempoxyz/tempo crates/primitives/src/header.rs TempoHeader);
// timestampMillis = timestamp*1000 + timestampMillisPart.
func tempoBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	outer := []headerField{quantityField("mainBlockGeneralGasLimit"), quantityField("sharedGasLimit"), quantityField("timestampMillisPart")}
	for k := range fields {
		if k != "timestampMillis" && k != "consensusContext" && !slices.ContainsFunc(outer, func(f headerField) bool { return f.key == k }) && !tempoHeaderLayout.allows(k) {
			return "", false
		}
	}
	ts, ok1 := jsonQuantity(fields["timestamp"])
	part, ok2 := jsonQuantity(fields["timestampMillisPart"])
	millis, ok3 := jsonQuantity(fields["timestampMillis"])
	if !ok1 || !ok2 || !ok3 || ts.Mul(ts, big.NewInt(1000)).Add(ts, part).Cmp(millis) != 0 {
		return "", false
	}
	var ctx map[string]json.RawMessage
	contextFields := []headerField{quantityField("epoch"), quantityField("view"), quantityField("parentView"), bytesField("proposer")}
	if present(fields, "consensusContext") {
		if json.Unmarshal(fields["consensusContext"], &ctx) != nil || len(ctx) != len(contextFields) {
			return "", false
		}
	}
	return rlpListHash(func(w rlp.EncoderBuffer) bool {
		for _, f := range outer {
			if !writeField(w, f, fields[f.key]) {
				return false
			}
		}
		header := w.List()
		if !tempoHeaderLayout.write(w, fields) {
			return false
		}
		w.ListEnd(header)
		if ctx == nil {
			return true
		}
		list := w.List()
		for _, f := range contextFields {
			if !writeField(w, f, ctx[f.key]) {
				return false
			}
		}
		w.ListEnd(list)
		return true
	})
}

// rootstockKeys are the JSON keys rskj serves; mixHash, nonce,
// cumulativeDifficulty, the merged-mining coinbase transaction and merkle
// proof are outside the hash.
var rootstockKeys = []string{
	"parentHash", "sha3Uncles", "miner", "stateRoot", "transactionsRoot", "receiptsRoot", "logsBloom",
	"difficulty", "number", "gasLimit", "gasUsed", "timestamp", "extraData", "paidFees", "minimumGasPrice",
	"rskPteEdges", "baseEvent", "bitcoinMergedMiningHeader", "hashForMergedMining",
	"mixHash", "nonce", "cumulativeDifficulty", "bitcoinMergedMiningCoinbaseTransaction", "bitcoinMergedMiningMerkleProof",
}

// rootstockBlockHash ports rskj BlockHeader.getEncoded with merged-mining
// fields and without merkle proof and coinbase (RSKIP92). The header version
// (0, 1, 2) and an empty or absent ummRoot are not in the JSON:
// hashForMergedMining[0:20], keccak256 of the encoding without the bitcoin
// header, picks them.
func rootstockBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	for k := range fields {
		if !slices.Contains(rootstockKeys, k) && !slices.Contains(rpcOnlyBlockKeys, k) {
			return "", false
		}
	}
	h, ok := parseRootstockHeader(fields)
	if !ok {
		return "", false
	}
	for version := byte(0); version <= 2; version++ {
		for _, umm := range [][]byte{{}, nil} {
			mm := crypto.Keccak256(h.encode(version, umm, false))
			if len(h.hashForMergedMining) >= 20 && string(mm[:20]) == string(h.hashForMergedMining[:20]) {
				return gethcommon.BytesToHash(crypto.Keccak256(h.encode(version, umm, true))).Hex(), true
			}
		}
	}
	return "", false
}

type rootstockHeader struct {
	plain               [7][]byte // parentHash … receiptsRoot, then logsBloom last
	difficulty, number  *big.Int
	gasLimit, gasUsed   *big.Int
	timestamp           *big.Int
	extraData           []byte
	paidFees, minGas    *big.Int
	uncles              uint64
	edges               []byte // little-endian int16s; nil when rskPteEdges is null
	baseEvent           []byte
	btcHeader           []byte
	hashForMergedMining []byte
}

func parseRootstockHeader(fields map[string]json.RawMessage) (*rootstockHeader, bool) {
	h := &rootstockHeader{}
	for i, k := range []string{"parentHash", "sha3Uncles", "miner", "stateRoot", "transactionsRoot", "receiptsRoot", "logsBloom"} {
		b, ok := jsonBytes(fields[k])
		if !ok {
			return nil, false
		}
		h.plain[i] = b
	}
	ok := true
	quantity := func(k string) *big.Int {
		if string(fields[k]) == `"0x"` {
			return new(big.Int)
		}
		v, good := jsonQuantity(fields[k])
		ok = ok && good
		return v
	}
	bytesOf := func(k string) []byte {
		if !present(fields, k) {
			return nil
		}
		b, good := jsonBytes(fields[k])
		ok = ok && good
		return b
	}
	h.difficulty, h.number = quantity("difficulty"), quantity("number")
	h.gasLimit, h.gasUsed, h.timestamp = quantity("gasLimit"), quantity("gasUsed"), quantity("timestamp")
	h.paidFees, h.minGas = quantity("paidFees"), quantity("minimumGasPrice")
	h.extraData, h.baseEvent = bytesOf("extraData"), bytesOf("baseEvent")
	h.btcHeader, h.hashForMergedMining = bytesOf("bitcoinMergedMiningHeader"), bytesOf("hashForMergedMining")
	var uncles []json.RawMessage
	if json.Unmarshal(fields["uncles"], &uncles) != nil {
		return nil, false
	}
	h.uncles = uint64(len(uncles))
	if present(fields, "rskPteEdges") {
		var edges []json.RawMessage
		if json.Unmarshal(fields["rskPteEdges"], &edges) != nil {
			return nil, false
		}
		h.edges = make([]byte, 0, 2*len(edges))
		for _, e := range edges {
			n, good := jsonUint64(e)
			if !good || n > 0x7fff {
				return nil, false
			}
			h.edges = append(h.edges, byte(n), byte(n>>8))
		}
	}
	return h, ok
}

func rlpOf(v any) rlp.RawValue {
	b, _ := rlp.EncodeToBytes(v) // only byte strings, integers and lists of RawValue
	return b
}

// extension is the v1/v2 logsBloom stand-in: RLP[version,
// keccak256(RLP[keccak256(bloom), baseEvent (v2), edges?])].
func (h *rootstockHeader) extension(version byte) []byte {
	inner := []rlp.RawValue{rlpOf(crypto.Keccak256(h.plain[6]))}
	if version == 2 {
		inner = append(inner, rlpOf(h.baseEvent))
	}
	if h.edges != nil {
		inner = append(inner, rlpOf(h.edges))
	}
	return rlpOf([]rlp.RawValue{rlpOf(version), rlpOf(crypto.Keccak256(rlpOf(inner)))})
}

func (h *rootstockHeader) encode(version byte, umm []byte, withBTCHeader bool) []byte {
	bloom := h.plain[6]
	if version > 0 {
		bloom = h.extension(version)
	}
	// rskj keeps gasLimit as Java BigInteger.toByteArray: signed, so a set top bit gains a 0x00.
	gasLimit := h.gasLimit.Bytes()
	if len(gasLimit) > 0 && gasLimit[0]&0x80 != 0 {
		gasLimit = append([]byte{0}, gasLimit...)
	}
	f := make([]rlp.RawValue, 0, 20)
	for _, b := range h.plain[:6] {
		f = append(f, rlpOf(b))
	}
	f = append(f, rlpOf(bloom), rlpOf(h.difficulty), rlpOf(h.number), rlpOf(gasLimit), rlpOf(h.gasUsed),
		rlpOf(h.timestamp), rlpOf(h.extraData), rlpOf(h.paidFees), rlpOf(h.minGas), rlpOf(h.uncles))
	if umm != nil {
		f = append(f, rlpOf(umm))
	}
	if version == 0 {
		if h.edges != nil {
			f = append(f, rlpOf(h.edges))
		}
		if len(h.baseEvent) > 0 {
			f = append(f, rlpOf(h.baseEvent))
		}
	}
	if withBTCHeader {
		f = append(f, rlpOf(h.btcHeader))
	}
	return rlpOf(f)
}

// etherlinkBlockHash: the Etherlink kernel hashes keccak256(RLP of 11 strings),
// each the hex text of a field: parentHash, stateRoot, transactionsRoot,
// receiptsRoot, logsBloom, miner, number (32-byte little-endian), gasLimit
// (8-byte little-endian), gasUsed (decimal text), timestamp (RFC 3339 text),
// and an empty extra data (tezos/tezos etherlink/kernel_latest/ethereum/src/block.rs).
// Its keys are all standard, so the format is bound to Etherlink's chain ids.
func etherlinkBlockHash(chainID int64, fields map[string]json.RawMessage) (string, bool) {
	if chainID != 42793 && chainID != 127823 {
		return "", false
	}
	for k := range fields {
		if _, ok := knownBlockFields[k]; !ok {
			return "", false
		}
	}
	number, ok1 := jsonQuantity(fields["number"])
	gasLimit, ok2 := jsonUint64(fields["gasLimit"])
	gasUsed, ok3 := jsonQuantity(fields["gasUsed"])
	ts, ok4 := jsonUint64(fields["timestamp"])
	if !ok1 || !ok2 || !ok3 || !ok4 || ts > 1<<62 {
		return "", false
	}
	items := make([]string, 0, 11)
	for _, k := range []string{"parentHash", "stateRoot", "transactionsRoot", "receiptsRoot", "logsBloom", "miner"} {
		d, ok := jsonHexDigits(fields[k])
		if !ok {
			return "", false
		}
		items = append(items, strings.ToLower(string(d)))
	}
	var num [32]byte
	number.FillBytes(num[:])
	slices.Reverse(num[:])
	var gl [8]byte
	binary.LittleEndian.PutUint64(gl[:], gasLimit)
	items = append(items,
		hex.EncodeToString(num[:]),
		hex.EncodeToString(gl[:]),
		hex.EncodeToString([]byte(gasUsed.String())),
		hex.EncodeToString([]byte(time.Unix(int64(ts), 0).UTC().Format(time.RFC3339))),
		"")
	enc, err := rlp.EncodeToBytes(items)
	if err != nil {
		return "", false
	}
	return crypto.Keccak256Hash(enc).Hex(), true
}

// auroraBlockHash: sha256(0x00 ‖ chainID as 32 bytes ‖ "aurora" ‖ number as
// 8 bytes), no header field hashed (aurora-engine engine/src/engine.rs
// compute_block_hash). Its keys are all standard, so the format is bound to
// Aurora's chain ids.
func auroraBlockHash(chainID int64, fields map[string]json.RawMessage) (string, bool) {
	if chainID != 1313161554 && chainID != 1313161555 {
		return "", false
	}
	for k := range fields {
		if _, ok := knownBlockFields[k]; !ok {
			return "", false
		}
	}
	number, ok := jsonUint64(fields["number"])
	if !ok {
		return "", false
	}
	var buf [1 + 32 + 6 + 8]byte
	binary.BigEndian.PutUint64(buf[25:33], uint64(chainID))
	copy(buf[33:39], "aurora")
	binary.BigEndian.PutUint64(buf[39:], number)
	sum := sha256.Sum256(buf[:])
	return gethcommon.Hash(sum).Hex(), true
}

// zksyncBlockHash: keccak256(number ‖ timestamp ‖ parentHash ‖ rolling), the
// first two as 32 bytes and rolling folding keccak256(rolling ‖ txHash) over
// the block's transactions from zero (matter-labs/zksync-era
// core/lib/types/src/block.rs L2BlockHasher, protocol v13 on).
func zksyncBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	zkKeys := []string{"l1BatchNumber", "l1BatchTimestamp", "sealFields"}
	for _, k := range zkKeys {
		if _, ok := fields[k]; !ok {
			return "", false
		}
	}
	for k := range fields {
		if _, ok := knownBlockFields[k]; !ok && !slices.Contains(zkKeys, k) {
			return "", false
		}
	}
	number, ok1 := jsonQuantity(fields["number"])
	ts, ok2 := jsonQuantity(fields["timestamp"])
	parent, ok3 := jsonBytes(fields["parentHash"])
	var txs []json.RawMessage
	if !ok1 || !ok2 || !ok3 || len(parent) != 32 || json.Unmarshal(fields["transactions"], &txs) != nil {
		return "", false
	}
	var rolling gethcommon.Hash
	for _, tx := range txs {
		raw := tx
		if len(tx) > 0 && tx[0] == '{' {
			var obj struct {
				Hash json.RawMessage `json:"hash"`
			}
			if json.Unmarshal(tx, &obj) != nil {
				return "", false
			}
			raw = obj.Hash
		}
		txHash, ok := jsonBytes(raw)
		if !ok || len(txHash) != 32 {
			return "", false
		}
		rolling = crypto.Keccak256Hash(rolling[:], txHash)
	}
	var digest [128]byte
	number.FillBytes(digest[0:32])
	ts.FillBytes(digest[32:64])
	copy(digest[64:96], parent)
	copy(digest[96:], rolling[:])
	return crypto.Keccak256Hash(digest[:]).Hex(), true
}
