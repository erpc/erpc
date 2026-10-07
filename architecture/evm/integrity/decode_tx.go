package integrity

import (
	"encoding/json"
	"errors"
	"math/big"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
)

// decodeTxSonic is gethtypes.Transaction.UnmarshalJSON (go-ethereum v1.17.5)
// with the structural parse done once, up front, instead of by encoding/json
// through reflection. Every field value still goes through geth's own
// decoder (hexutil.Big/Uint64/Bytes, common.Address/Hash, and encoding/json
// for the AccessList and slice fields), and the legacy, EIP-2930 and
// EIP-1559 paths are ported line for line, including the yParity/v rules and
// sanityCheckSignature. Blob and SetCode transactions, unknown types, and
// any input outside the plain subset fall back to geth verbatim.
//
// It also returns the claimed "hash" string, exactly as the original
// json.Unmarshal(raw, &struct{Hash string}) read it.
//
// The contract (accept/reject identical to geth, identical hash when
// accepted) is enforced by TestDecodeTxSonicParity and FuzzDecodeTxSonic.
func decodeTxSonic(raw []byte) (*gethtypes.Transaction, string, error) {
	if !isPlainJSON(raw) || !common.SonicCfg.Valid(raw) {
		return decodeTxGeth(raw)
	}
	st := splitTxObject(util.B2Str(raw))
	if !st.ok {
		return decodeTxGeth(raw)
	}
	return st.decode()
}

// decodeTxGeth is the original per-transaction decode: geth's decoder plus
// the claimed hash read through encoding/json.
func decodeTxGeth(raw []byte) (*gethtypes.Transaction, string, error) {
	tx := new(gethtypes.Transaction)
	if err := tx.UnmarshalJSON(raw); err != nil {
		return nil, "", err
	}
	var meta struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, "", err
	}
	return tx, meta.Hash, nil
}

// Transaction object keys the decoders read: geth's txJSON fields plus the
// Tx view's. Matching is ASCII case-insensitive, as encoding/json and sonic
// both do on plain input.
const (
	kType = iota
	kChainID
	kNonce
	kTo
	kGas
	kGasPrice
	kMaxPriorityFeePerGas
	kMaxFeePerGas
	kMaxFeePerBlobGas
	kValue
	kInput
	kAccessList
	kBlobVersionedHashes
	kAuthorizationList
	kV
	kR
	kS
	kYParity
	kBlobs
	kCommitments
	kProofs
	kHash
	kFrom
	kBlockHash
	kBlockNumber
	kTransactionIndex
	numTxKeys
)

var txKeyNames = [numTxKeys]string{
	kType: "type", kChainID: "chainid", kNonce: "nonce", kTo: "to", kGas: "gas",
	kGasPrice: "gasprice", kMaxPriorityFeePerGas: "maxpriorityfeepergas",
	kMaxFeePerGas: "maxfeepergas", kMaxFeePerBlobGas: "maxfeeperblobgas",
	kValue: "value", kInput: "input", kAccessList: "accesslist",
	kBlobVersionedHashes: "blobversionedhashes", kAuthorizationList: "authorizationlist",
	kV: "v", kR: "r", kS: "s", kYParity: "yparity", kBlobs: "blobs",
	kCommitments: "commitments", kProofs: "proofs", kHash: "hash",
	kFrom: "from", kBlockHash: "blockhash", kBlockNumber: "blocknumber",
	kTransactionIndex: "transactionindex",
}

func txKeyID(key string) int {
	for id, name := range txKeyNames {
		if asciiEqualFold(key, name) {
			return id
		}
	}
	return -1
}

// splitTx is one transaction object split into its raw member values. It
// is only built from text already proven to be valid JSON in the plain
// subset, so a value is "" (absent), "null", a quoted string with no escapes,
// or any other valid JSON value.
type splitTx struct {
	raw    string
	fields [numTxKeys]string
	// ok: raw is an object with no two keys equal under ASCII case folding.
	// Duplicate keys are where the decoders this replaces disagree (and
	// encoding/json runs a field decoder per occurrence), so they take the
	// original path.
	ok bool
}

func splitTxObject(raw string) *splitTx {
	st := &splitTx{raw: raw}
	var keys [32]string
	seen := keys[:0]
	dup := false
	isObj := forEachPair(raw, 0, func(k, v string) {
		for _, p := range seen {
			if asciiFoldEq(p, k) {
				dup = true
			}
		}
		seen = append(seen, k)
		if id := txKeyID(k); id >= 0 {
			st.fields[id] = v
		}
	})
	st.ok = isObj && !dup
	return st
}

func isNullRaw(v string) bool { return v == "" || v == "null" }

func rawBytes(v string) []byte { return util.S2Bytes(v) }

// The helpers mirror how encoding/json fills txJSON: a pointer field is left
// nil for an absent or null value and otherwise goes to the field type's
// UnmarshalJSON.
func optBig(v string) (*big.Int, error) {
	if isNullRaw(v) {
		return nil, nil
	}
	var b hexutil.Big
	if err := b.UnmarshalJSON(rawBytes(v)); err != nil {
		return nil, err
	}
	return (*big.Int)(&b), nil
}

func optU64(v string) (*uint64, error) {
	if isNullRaw(v) {
		return nil, nil
	}
	var u hexutil.Uint64
	if err := u.UnmarshalJSON(rawBytes(v)); err != nil {
		return nil, err
	}
	x := uint64(u)
	return &x, nil
}

// optSlice decodes a slice field with encoding/json (null leaves it nil).
func optSlice(v string, out any) error {
	if isNullRaw(v) {
		return nil
	}
	return json.Unmarshal(rawBytes(v), out)
}

var (
	errTxYParity        = errors.New("'yParity' field must be 0 or 1")
	errTxVYParity       = errors.New("'v' and 'yParity' fields do not match")
	errTxVYParityAbsent = errors.New("missing 'yParity' or 'v' field in transaction")
)

func errTxMissing(field string) error {
	return errors.New("missing required field '" + field + "' in transaction")
}

// claimedHash is the transaction's "hash" member as a string, as the
// original json.Unmarshal into struct{Hash string} read it (no escapes can
// occur in plain input). Only meaningful once decode accepted the tx, which
// guarantees the member is absent or a valid hash string.
func (st *splitTx) claimedHash() string {
	v := st.fields[kHash]
	if len(v) >= 2 && v[0] == '"' {
		return v[1 : len(v)-1]
	}
	return ""
}

// decode ports Transaction.UnmarshalJSON over the split fields. It must only
// be called when st.ok.
func (st *splitTx) decode() (*gethtypes.Transaction, string, error) {
	f := &st.fields

	// Phase 1: decode every field, as encoding/json does before geth looks at
	// any of them. A failure in any present field rejects the transaction
	// whatever its type.
	var typ hexutil.Uint64
	if f[kType] != "" {
		// Non-pointer field: encoding/json hands even a null to UnmarshalJSON.
		if err := typ.UnmarshalJSON(rawBytes(f[kType])); err != nil {
			return nil, "", err
		}
	}
	if f[kHash] != "" {
		var h gethcommon.Hash
		if err := h.UnmarshalJSON(rawBytes(f[kHash])); err != nil {
			return nil, "", err
		}
	}
	chainID, err := optBig(f[kChainID])
	if err != nil {
		return nil, "", err
	}
	nonce, err := optU64(f[kNonce])
	if err != nil {
		return nil, "", err
	}
	var to *gethcommon.Address
	if !isNullRaw(f[kTo]) {
		to = new(gethcommon.Address)
		if err := to.UnmarshalJSON(rawBytes(f[kTo])); err != nil {
			return nil, "", err
		}
	}
	gas, err := optU64(f[kGas])
	if err != nil {
		return nil, "", err
	}
	gasPrice, err := optBig(f[kGasPrice])
	if err != nil {
		return nil, "", err
	}
	tip, err := optBig(f[kMaxPriorityFeePerGas])
	if err != nil {
		return nil, "", err
	}
	feeCap, err := optBig(f[kMaxFeePerGas])
	if err != nil {
		return nil, "", err
	}
	if _, err := optBig(f[kMaxFeePerBlobGas]); err != nil {
		return nil, "", err
	}
	value, err := optBig(f[kValue])
	if err != nil {
		return nil, "", err
	}
	var input *hexutil.Bytes
	if !isNullRaw(f[kInput]) {
		input = new(hexutil.Bytes)
		if err := input.UnmarshalJSON(rawBytes(f[kInput])); err != nil {
			return nil, "", err
		}
	}
	var accessList *gethtypes.AccessList
	if !isNullRaw(f[kAccessList]) {
		accessList = new(gethtypes.AccessList)
		if f[kAccessList] == "[]" {
			*accessList = gethtypes.AccessList{}
		} else if err := json.Unmarshal(rawBytes(f[kAccessList]), accessList); err != nil {
			return nil, "", err
		}
	}
	var blobHashes []gethcommon.Hash
	if err := optSlice(f[kBlobVersionedHashes], &blobHashes); err != nil {
		return nil, "", err
	}
	var authList []gethtypes.SetCodeAuthorization
	if err := optSlice(f[kAuthorizationList], &authList); err != nil {
		return nil, "", err
	}
	v, err := optBig(f[kV])
	if err != nil {
		return nil, "", err
	}
	r, err := optBig(f[kR])
	if err != nil {
		return nil, "", err
	}
	s, err := optBig(f[kS])
	if err != nil {
		return nil, "", err
	}
	yParity, err := optU64(f[kYParity])
	if err != nil {
		return nil, "", err
	}
	var blobs []kzg4844.Blob
	if err := optSlice(f[kBlobs], &blobs); err != nil {
		return nil, "", err
	}
	var commitments []kzg4844.Commitment
	if err := optSlice(f[kCommitments], &commitments); err != nil {
		return nil, "", err
	}
	var proofs []kzg4844.Proof
	if err := optSlice(f[kProofs], &proofs); err != nil {
		return nil, "", err
	}

	// txJSON.yParityValue
	yParityValue := func() (*big.Int, error) {
		if yParity != nil {
			val := *yParity
			if val != 0 && val != 1 {
				return nil, errTxYParity
			}
			bv := new(big.Int).SetUint64(val)
			if v != nil && v.Cmp(bv) != 0 {
				return nil, errTxVYParity
			}
			return bv, nil
		}
		if v != nil {
			return v, nil
		}
		return nil, errTxVYParityAbsent
	}

	// Phase 2: per-type validation, in geth's order.
	var inner gethtypes.TxData
	switch uint64(typ) {
	case gethtypes.LegacyTxType:
		itx := &gethtypes.LegacyTx{To: to}
		switch {
		case nonce == nil:
			return nil, "", errTxMissing("nonce")
		case gas == nil:
			return nil, "", errTxMissing("gas")
		case gasPrice == nil:
			return nil, "", errTxMissing("gasPrice")
		case value == nil:
			return nil, "", errTxMissing("value")
		case input == nil:
			return nil, "", errTxMissing("input")
		case r == nil:
			return nil, "", errTxMissing("r")
		case s == nil:
			return nil, "", errTxMissing("s")
		case v == nil:
			return nil, "", errTxMissing("v")
		}
		itx.Nonce, itx.Gas, itx.GasPrice, itx.Value, itx.Data = *nonce, *gas, gasPrice, value, *input
		itx.V, itx.R, itx.S = v, r, s
		if v.Sign() != 0 || r.Sign() != 0 || s.Sign() != 0 {
			if err := sanityCheckSignature(v, r, s, true); err != nil {
				return nil, "", err
			}
		}
		inner = itx

	case gethtypes.AccessListTxType:
		itx := &gethtypes.AccessListTx{To: to}
		switch {
		case chainID == nil:
			return nil, "", errTxMissing("chainId")
		case nonce == nil:
			return nil, "", errTxMissing("nonce")
		case gas == nil:
			return nil, "", errTxMissing("gas")
		case gasPrice == nil:
			return nil, "", errTxMissing("gasPrice")
		case value == nil:
			return nil, "", errTxMissing("value")
		case input == nil:
			return nil, "", errTxMissing("input")
		}
		itx.ChainID, itx.Nonce, itx.Gas, itx.GasPrice, itx.Value, itx.Data = chainID, *nonce, *gas, gasPrice, value, *input
		if accessList != nil {
			itx.AccessList = *accessList
		}
		switch {
		case r == nil:
			return nil, "", errTxMissing("r")
		case s == nil:
			return nil, "", errTxMissing("s")
		}
		itx.R, itx.S = r, s
		if itx.V, err = yParityValue(); err != nil {
			return nil, "", err
		}
		if itx.V.Sign() != 0 || r.Sign() != 0 || s.Sign() != 0 {
			if err := sanityCheckSignature(itx.V, r, s, false); err != nil {
				return nil, "", err
			}
		}
		inner = itx

	case gethtypes.DynamicFeeTxType:
		itx := &gethtypes.DynamicFeeTx{To: to}
		switch {
		case chainID == nil:
			return nil, "", errTxMissing("chainId")
		case nonce == nil:
			return nil, "", errTxMissing("nonce")
		case gas == nil:
			return nil, "", errTxMissing("gas")
		case tip == nil:
			return nil, "", errTxMissing("maxPriorityFeePerGas")
		case feeCap == nil:
			return nil, "", errTxMissing("maxFeePerGas")
		case value == nil:
			return nil, "", errTxMissing("value")
		case input == nil:
			return nil, "", errTxMissing("input")
		}
		itx.ChainID, itx.Nonce, itx.Gas, itx.GasTipCap, itx.GasFeeCap = chainID, *nonce, *gas, tip, feeCap
		itx.Value, itx.Data = value, *input
		if accessList != nil {
			itx.AccessList = *accessList
		}
		switch {
		case r == nil:
			return nil, "", errTxMissing("r")
		case s == nil:
			return nil, "", errTxMissing("s")
		}
		itx.R, itx.S = r, s
		if itx.V, err = yParityValue(); err != nil {
			return nil, "", err
		}
		if itx.V.Sign() != 0 || r.Sign() != 0 || s.Sign() != 0 {
			if err := sanityCheckSignature(itx.V, r, s, false); err != nil {
				return nil, "", err
			}
		}
		inner = itx

	default:
		// Blob, SetCode and unknown types: geth verbatim. Rare on the hot path,
		// and it keeps their uint256 overflow rules exact without a second port.
		tx := new(gethtypes.Transaction)
		if err := tx.UnmarshalJSON(rawBytes(st.raw)); err != nil {
			return nil, "", err
		}
		return tx, st.claimedHash(), nil
	}
	return gethtypes.NewTx(inner), st.claimedHash(), nil
}

// sanityCheckSignature mirrors gethtypes.sanityCheckSignature (unexported in
// geth), including isProtectedV and deriveChainId.
func sanityCheckSignature(v, r, s *big.Int, maybeProtected bool) error {
	if isProtectedV(v) && !maybeProtected {
		return gethtypes.ErrUnexpectedProtection
	}
	var plainV byte
	if isProtectedV(v) {
		chainID := deriveChainID(v).Uint64()
		plainV = byte(v.Uint64() - 35 - 2*chainID)
	} else if maybeProtected {
		plainV = byte(v.Uint64() - 27)
	} else {
		plainV = byte(v.Uint64())
	}
	if !crypto.ValidateSignatureValues(plainV, r, s, false) {
		return gethtypes.ErrInvalidSig
	}
	return nil
}

func isProtectedV(v *big.Int) bool {
	if v.BitLen() <= 8 {
		x := v.Uint64()
		return x != 27 && x != 28 && x != 1 && x != 0
	}
	return true
}

func deriveChainID(v *big.Int) *big.Int {
	if v.BitLen() <= 64 {
		x := v.Uint64()
		if x == 27 || x == 28 {
			return new(big.Int)
		}
		return new(big.Int).SetUint64((x - 35) / 2)
	}
	c := new(big.Int).Sub(v, big.NewInt(35))
	return c.Rsh(c, 1)
}

// txView builds the lightweight Tx view from the split fields, matching the
// original map -> Marshal -> Unmarshal(Tx) round trip: a view field takes a
// string's contents, stays empty for an absent or null member, and any other
// value type makes the whole entry undecodable (dropped from the views).
func (st *splitTx) txView() (Tx, bool) {
	var t Tx
	for _, f := range [...]struct {
		id  int
		dst *string
	}{
		{kHash, &t.Hash}, {kFrom, &t.From}, {kTo, &t.To}, {kGas, &t.Gas},
		{kBlockHash, &t.BlockHash}, {kBlockNumber, &t.BlockNumber},
		{kTransactionIndex, &t.TransactionIndex},
	} {
		v := st.fields[f.id]
		switch {
		case isNullRaw(v):
		case v[0] == '"':
			*f.dst = v[1 : len(v)-1]
		default:
			return Tx{}, false
		}
	}
	return t, true
}
