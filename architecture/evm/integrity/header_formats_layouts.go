package integrity

import (
	"encoding/json"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Header formats hashed as keccak256(RLP(field list)) in an order of their own.

// corethLayout is coreth's HeaderSerializable, Avalanche C-Chain and Flare
// alike (ava-labs/avalanchego graft/coreth/plugin/evm/customtypes/header_ext.go;
// flare-foundation/go-flare adds pChainHeight last).
var corethLayout = rlpLayout{
	required: slices.Concat(gethBaseFields, []headerField{bytesField("extDataHash")}),
	optional: []headerField{
		quantityField("baseFeePerGas"), quantityField("extDataGasUsed"), quantityField("blockGasCost"),
		quantityField("blobGasUsed"), quantityField("excessBlobGas"), bytesField("parentBeaconBlockRoot"),
		quantityField("timestampMilliseconds"), quantityField("minDelayExcess"), quantityField("targetExponent"),
		quantityField("minPriceExponent"), quantityField("settledHeight"), quantityField("settledGasUnix"),
		quantityField("settledGasNumerator"), quantityField("settledExcess"), quantityField("pChainHeight"),
	},
	unhashed: []string{"blockExtraData"},
}

// corethBlockHash: blockExtraData is the block body's atomic data, hashed as
// extDataHash = keccak256(RLP(blockExtraData)) (coreth CalcExtDataHash).
func corethBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	ext, ok := jsonBytes(fields["blockExtraData"])
	if !ok {
		return "", false
	}
	enc, err := rlp.EncodeToBytes(ext)
	if err != nil || !sameHash(fields["extDataHash"], crypto.Keccak256(enc)) {
		return "", false
	}
	return corethLayout.hash(fields)
}

// sameHash reports whether raw is the JSON hex of sum.
func sameHash(raw json.RawMessage, sum []byte) bool {
	b, ok := jsonBytes(raw)
	return ok && string(b) == string(sum)
}

// subnetEVMLayout: ava-labs/avalanchego graft/subnet-evm/plugin/evm/customtypes/header_ext.go.
var subnetEVMLayout = rlpLayout{
	required: gethBaseFields,
	optional: []headerField{
		quantityField("baseFeePerGas"), quantityField("blockGasCost"), quantityField("blobGasUsed"),
		quantityField("excessBlobGas"), bytesField("parentBeaconBlockRoot"),
		quantityField("timestampMilliseconds"), quantityField("minDelayExcess"),
	},
}

func subnetEVMBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	if !present(fields, "blockGasCost") {
		return "", false
	}
	return subnetEVMLayout.hash(fields)
}

// berachainLayout: the Prague header plus prev_proposer_pubkey last
// (berachain/bera-reth src/primitives/header.rs).
var berachainLayout = rlpLayout{
	required: gethBaseFields,
	optional: []headerField{
		quantityField("baseFeePerGas"), bytesField("withdrawalsRoot"), quantityField("blobGasUsed"),
		quantityField("excessBlobGas"), bytesField("parentBeaconBlockRoot"), bytesField("requestsHash"),
		bytesField("parentProposerPubkey"),
	},
}

func berachainBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	if !present(fields, "parentProposerPubkey") {
		return "", false
	}
	return berachainLayout.hash(fields)
}

// xdposLayout: XDC and Viction hash the seal in validator, after the
// masternode list and penalties (XinFinOrg/XDPoSChain core/types/block.go).
var xdposLayout = rlpLayout{
	required: slices.Concat(gethBaseFields, []headerField{bytesField("validators"), bytesField("validator"), bytesField("penalties")}),
	optional: []headerField{quantityField("baseFeePerGas")},
}

func xdposBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	return xdposLayout.hash(fields)
}

// fuseLayout: Nethermind AuRa hashes step and signature where mixHash and
// nonce stand (Nethermind HeaderDecoder); author repeats miner.
var fuseLayout = rlpLayout{
	required: slices.Concat(gethBaseFields[:13], []headerField{quantityField("step"), bytesField("signature")}),
	optional: []headerField{quantityField("baseFeePerGas")},
	unhashed: []string{"author"},
}

func fuseBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	if !sameBytes(fields, "author", "miner") {
		return "", false
	}
	return fuseLayout.hash(fields)
}

// frontierLayout is rust-ethereum's 15-field Header as Frontier serves it:
// mixHash is always zero and left out, timestamp is in seconds while the
// header holds milliseconds, author repeats miner, and baseFeePerGas comes
// from the fee pallet, outside the header (polkadot-evm/frontier fc-rpc).
var frontierLayout = rlpLayout{
	required: slices.Concat(gethBaseFields[:13], gethBaseFields[14:]),
	unhashed: []string{"author", "baseFeePerGas"},
}

func frontierBlockHash(_ int64, fields map[string]json.RawMessage) (string, bool) {
	ts, ok := jsonQuantity(fields["timestamp"])
	if !ok || !sameBytes(fields, "author", "miner") {
		return "", false
	}
	for k := range fields {
		if !frontierLayout.allows(k) {
			return "", false
		}
	}
	ms := ts.Mul(ts, big.NewInt(1000))
	return rlpListHash(func(w rlp.EncoderBuffer) bool {
		for _, f := range frontierLayout.required {
			switch f.key {
			case "timestamp":
				w.WriteBigInt(ms)
			case "nonce":
				w.WriteBytes(make([]byte, 32)) // mixHash
				if !writeField(w, f, fields[f.key]) {
					return false
				}
			default:
				if !writeField(w, f, fields[f.key]) {
					return false
				}
			}
		}
		return true
	})
}

// lightlinkLayout: LightLink hashes the pre-London header with an empty
// extraData while its RPC serves 32 zero bytes (lightlink-network
// hummingbird-contracts ChainOracle.sol). Its keys are all standard, so the
// format is bound to LightLink's chain ids.
var lightlinkLayout = rlpLayout{required: gethBaseFields}

func lightlinkBlockHash(chainID int64, fields map[string]json.RawMessage) (string, bool) {
	if chainID != 1890 && chainID != 1891 {
		return "", false
	}
	extra, ok1 := jsonBytes(fields["extraData"])
	receipts, ok2 := jsonBytes(fields["receiptsRoot"])
	if !ok1 || !ok2 || !allZero(extra) || allZero(receipts) {
		// Early blocks report a zero receiptsRoot whose real value is not served.
		return "", false
	}
	std := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		std[k] = v
	}
	std["extraData"] = json.RawMessage(`"0x"`)
	return lightlinkLayout.hash(std)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
