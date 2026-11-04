package integrity

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"slices"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Shared pieces of the <name>BlockHash header formats: JSON value decoding,
// the go-ethereum header with a family's derived keys dropped, and RLP
// layouts for families whose header is a field list of their own.

// rpcOnlyBlockKeys are the eth_getBlock* keys an RPC adds beside the header;
// no format hashes them.
var rpcOnlyBlockKeys = []string{"hash", "size", "totalDifficulty", "transactions", "uncles", "withdrawals"}

// jsonHexDigits returns the digits of a JSON string "0x…".
func jsonHexDigits(raw json.RawMessage) ([]byte, bool) {
	n := len(raw)
	if n < 4 || raw[0] != '"' || raw[n-1] != '"' || raw[1] != '0' || raw[2] != 'x' {
		return nil, false
	}
	return raw[3 : n-1], true
}

// jsonBytes decodes a JSON "0x…" byte string; "0x" is empty.
func jsonBytes(raw json.RawMessage) ([]byte, bool) {
	d, ok := jsonHexDigits(raw)
	if !ok || len(d)%2 != 0 {
		return nil, false
	}
	b := make([]byte, len(d)/2)
	if _, err := hex.Decode(b, d); err != nil {
		return nil, false
	}
	return b, true
}

// jsonQuantity decodes a non-negative integer of at most 256 bits sent as a
// JSON "0x…" string or a JSON number.
func jsonQuantity(raw json.RawMessage) (*big.Int, bool) {
	digits, base := []byte(raw), 10
	if d, ok := jsonHexDigits(raw); ok {
		digits, base = d, 16
	}
	if len(digits) == 0 || len(digits) > 78 {
		return nil, false
	}
	for _, c := range digits {
		isDec := c >= '0' && c <= '9'
		isHex := c|0x20 >= 'a' && c|0x20 <= 'f'
		if !isDec && !(base == 16 && isHex) {
			return nil, false
		}
	}
	v, ok := new(big.Int).SetString(string(digits), base)
	if !ok || v.BitLen() > 256 {
		return nil, false
	}
	return v, true
}

// jsonUint64 decodes a quantity that fits 64 bits.
func jsonUint64(raw json.RawMessage) (uint64, bool) {
	v, ok := jsonQuantity(raw)
	if !ok || !v.IsUint64() {
		return 0, false
	}
	return v.Uint64(), true
}

// present reports whether fields carries key with a non-null value.
func present(fields map[string]json.RawMessage, key string) bool {
	v, ok := fields[key]
	return ok && string(v) != "null"
}

// sameBytes reports whether both keys hold the same parsable byte string.
func sameBytes(fields map[string]json.RawMessage, a, b string) bool {
	x, ok1 := jsonBytes(fields[a])
	y, ok2 := jsonBytes(fields[b])
	return ok1 && ok2 && string(x) == string(y)
}

// gethHeaderHash is the go-ethereum header hash of a standard header whose
// JSON also carries derived: keys outside the header, computed from hashed
// fields or kept beside the header. It needs every derived key and refuses
// any other key go-ethereum does not encode.
func gethHeaderHash(fields map[string]json.RawMessage, derived ...string) (string, bool) {
	std := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		if _, ok := knownBlockFields[k]; ok {
			std[k] = v
		} else if !slices.Contains(derived, k) {
			return "", false
		}
	}
	if len(fields)-len(std) != len(derived) {
		return "", false
	}
	raw, err := json.Marshal(std)
	if err != nil {
		return "", false
	}
	var h gethtypes.Header
	if h.UnmarshalJSON(raw) != nil {
		return "", false
	}
	return h.Hash().Hex(), true
}

// headerField is one hashed field: its JSON key, and whether RLP holds it as
// an integer (a quantity) or as a byte string.
type headerField struct {
	key      string
	quantity bool
}

func bytesField(key string) headerField    { return headerField{key: key} }
func quantityField(key string) headerField { return headerField{key: key, quantity: true} }

// gethBaseFields are the 15 fields every go-ethereum-lineage header starts with.
var gethBaseFields = []headerField{
	bytesField("parentHash"), bytesField("sha3Uncles"), bytesField("miner"),
	bytesField("stateRoot"), bytesField("transactionsRoot"), bytesField("receiptsRoot"),
	bytesField("logsBloom"), quantityField("difficulty"), quantityField("number"),
	quantityField("gasLimit"), quantityField("gasUsed"), quantityField("timestamp"),
	bytesField("extraData"), bytesField("mixHash"), bytesField("nonce"),
}

// rlpLayout is a header hashed as keccak256 of the RLP list of its fields:
// required ones in order, then optional trailing ones under go-ethereum's
// rlp:"optional" rule (an absent field before a present one encodes empty).
type rlpLayout struct {
	required []headerField
	optional []headerField
	unhashed []string // the family's JSON keys outside the hash, beside rpcOnlyBlockKeys
}

func (l *rlpLayout) allows(key string) bool {
	isKey := func(f headerField) bool { return f.key == key }
	return slices.ContainsFunc(l.required, isKey) || slices.ContainsFunc(l.optional, isKey) ||
		slices.Contains(l.unhashed, key) || slices.Contains(rpcOnlyBlockKeys, key)
}

// write appends l's fields to w; false when a required field is missing or a
// value does not parse.
func (l *rlpLayout) write(w rlp.EncoderBuffer, fields map[string]json.RawMessage) bool {
	for _, f := range l.required {
		if !writeField(w, f, fields[f.key]) {
			return false
		}
	}
	end := 0
	for i, f := range l.optional {
		if present(fields, f.key) {
			end = i + 1
		}
	}
	for _, f := range l.optional[:end] {
		if !present(fields, f.key) {
			w.WriteBytes(nil)
		} else if !writeField(w, f, fields[f.key]) {
			return false
		}
	}
	return true
}

// hash is keccak256(RLP(l's fields)); false when fields carry a key l does
// not know.
func (l *rlpLayout) hash(fields map[string]json.RawMessage) (string, bool) {
	for k := range fields {
		if !l.allows(k) {
			return "", false
		}
	}
	return rlpListHash(func(w rlp.EncoderBuffer) bool { return l.write(w, fields) })
}

func writeField(w rlp.EncoderBuffer, f headerField, raw json.RawMessage) bool {
	if f.quantity {
		v, ok := jsonQuantity(raw)
		if ok {
			w.WriteBigInt(v)
		}
		return ok
	}
	b, ok := jsonBytes(raw)
	if ok {
		w.WriteBytes(b)
	}
	return ok
}

// rlpListHash is keccak256 of the RLP list that body writes, as 0x hex.
func rlpListHash(body func(rlp.EncoderBuffer) bool) (string, bool) {
	h := crypto.NewKeccakState()
	w := rlp.NewEncoderBuffer(h)
	list := w.List()
	ok := body(w)
	w.ListEnd(list)
	if err := w.Flush(); err != nil || !ok {
		return "", false
	}
	var sum gethcommon.Hash
	if _, err := h.Read(sum[:]); err != nil {
		return "", false
	}
	return sum.Hex(), true
}
