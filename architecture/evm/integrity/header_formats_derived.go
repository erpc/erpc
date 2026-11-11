package integrity

import (
	"encoding/binary"
	"encoding/json"
	"math/big"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Header formats that are the standard go-ethereum header plus JSON keys
// outside it. A key derived from hashed fields is checked against them, so a
// verified hash covers it too; a mismatch means the format does not apply.

// arbitrumNitroBlockHash: Nitro keeps sendRoot in extraData and sendCount,
// l1BlockNumber in mixHash bytes 0-8 and 8-16 (OffchainLabs/go-ethereum
// core/types/arb_types.go DeserializeHeaderExtraInformation).
func arbitrumNitroBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	mix, ok := jsonBytes(fields["mixHash"])
	if !ok || len(mix) != 32 || !sameBytes(fields, "sendRoot", "extraData") {
		return "", false
	}
	sendCount, ok1 := jsonUint64(fields["sendCount"])
	l1Block, ok2 := jsonUint64(fields["l1BlockNumber"])
	if !ok1 || !ok2 || binary.BigEndian.Uint64(mix[0:8]) != sendCount || binary.BigEndian.Uint64(mix[8:16]) != l1Block {
		return "", false
	}
	return gethHeaderHash(fields, "l1BlockNumber", "sendCount", "sendRoot")
}

// bscBlockHash: milliTimestamp = timestamp*1000 + mixHash as an integer
// (bnb-chain/bsc core/types/block.go MilliTimestamp); also core and chiliz.
func bscBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	mix, ok := jsonBytes(fields["mixHash"])
	if !ok || !milliTimestampIs(fields, new(big.Int).SetBytes(mix)) {
		return "", false
	}
	return gethHeaderHash(fields, "milliTimestamp")
}

// opbnbBlockHash: milliTimestamp = timestamp*1000 + mixHash[0:2]
// (bnb-chain/op-geth core/types/block.go MilliTimestamp).
func opbnbBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	mix, ok := jsonBytes(fields["mixHash"])
	if !ok || len(mix) < 2 || !milliTimestampIs(fields, new(big.Int).SetBytes(mix[:2])) {
		return "", false
	}
	return gethHeaderHash(fields, "milliTimestamp")
}

// milliTimestampIs reports whether milliTimestamp = timestamp*1000 + millis.
func milliTimestampIs(fields map[string]json.RawMessage, millis *big.Int) bool {
	ts, ok1 := jsonQuantity(fields["timestamp"])
	ms, ok2 := jsonQuantity(fields["milliTimestamp"])
	return ok1 && ok2 && ts.Mul(ts, big.NewInt(1000)).Add(ts, millis).Cmp(ms) == 0
}

// skaleBlockHash: skaled's author is the miner, and seedHash and boundary are
// Ethash values of number and difficulty (skaled libethashseal Ethash::jsInfo).
func skaleBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	number, ok1 := jsonUint64(fields["number"])
	difficulty, ok2 := jsonQuantity(fields["difficulty"])
	seed, ok3 := jsonBytes(fields["seedHash"])
	boundary, ok4 := jsonBytes(fields["boundary"])
	if !ok1 || !ok2 || !ok3 || !ok4 || !sameBytes(fields, "author", "miner") {
		return "", false
	}
	want := new(big.Int)
	if difficulty.Sign() > 0 {
		want.Lsh(big.NewInt(1), 256).Div(want, difficulty)
	}
	if want.Cmp(new(big.Int).SetBytes(boundary)) != 0 {
		return "", false
	}
	var s gethcommon.Hash
	for n := number / 30000; n > 0; n-- {
		s = crypto.Keccak256Hash(s[:])
	}
	if string(s[:]) != string(seed) {
		return "", false
	}
	return gethHeaderHash(fields, "author", "boundary", "seedHash")
}

// sonicBlockHash: timestampNano = timestamp*1e9 + extraData[0:4]
// (0xsoniclabs/sonic inter/block.go EncodeExtraData); epoch is RPC metadata.
func sonicBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	extra, ok1 := jsonBytes(fields["extraData"])
	ts, ok2 := jsonQuantity(fields["timestamp"])
	nanos, ok3 := jsonQuantity(fields["timestampNano"])
	if !ok1 || !ok2 || !ok3 || len(extra) < 4 {
		return "", false
	}
	ts.Mul(ts, big.NewInt(1_000_000_000)).Add(ts, big.NewInt(int64(binary.BigEndian.Uint32(extra[:4]))))
	if ts.Cmp(nanos) != 0 {
		return "", false
	}
	return gethHeaderHash(fields, "epoch", "timestampNano")
}

// morphBlockHash: nextL1MsgIndex is beside the hashed header
// (morph-l2/go-ethereum core/types/block.go headerHashing).
func morphBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	return gethHeaderHash(fields, "nextL1MsgIndex")
}

// citreaBlockHash: l1FeeRate is beside the header in citrea's SealedBlock.
func citreaBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	return gethHeaderHash(fields, "l1FeeRate")
}

// megaethBlockHash: the mini-block offsets, count and sequencer signature are
// block metadata beside the standard header.
func megaethBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	return gethHeaderHash(fields, "miniBlockCount", "miniBlockOffset", "signature", "txOffset")
}

// baseTimestampMsBlockHash: timestampMs = timestamp*1000 plus the millisecond
// part of the block's BaseTime deposit transaction, outside the header
// (base/base crates/common/consensus/src/base_time.rs).
func baseTimestampMsBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	ts, ok1 := jsonQuantity(fields["timestamp"])
	ms, ok2 := jsonQuantity(fields["timestampMs"])
	if !ok1 || !ok2 || new(big.Int).Div(ms, big.NewInt(1000)).Cmp(ts) != 0 {
		return "", false
	}
	return gethHeaderHash(fields, "timestampMs")
}
