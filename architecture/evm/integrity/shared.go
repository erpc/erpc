package integrity

import (
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// Shared decode for eth_getBlockByNumber / eth_getBlockByHash.
//
// Every block check used to re-parse the response: sonic decoded the header
// twice (Decoded.Header and schemaConformance), encoding/json decoded the
// whole block in blockHashRecompute (twice) and transactionsRootRecompute,
// and every transaction went through a map[string]any -> Marshal -> Unmarshal
// round trip for the typed view. Now one pass per response produces all of
// it, and each check reads from that pass.
//
// Exactness. The checks' verdicts must stay identical to the per-check
// decoders they replace (sonic for the header and tx views, encoding/json
// for the recomputes). Those decoders disagree with each other on
// pathological input: control characters, invalid UTF-8, escapes, key case
// folding of non-ASCII keys, duplicate keys, float overflow, very deep
// nesting, and invalid JSON inside skipped fields. So the fast path only
// runs on documents where they provably agree:
//
//   - the whole document is printable ASCII with no backslash (isPlainJSON),
//     so there are no escapes, no UTF-8, no control bytes, and key matching
//     is plain ASCII case folding in both decoders;
//   - it is valid JSON (sonic's validator; on plain input it agrees with
//     encoding/json's, and anything it rejects takes the exact path);
//   - the top level is an object with no two keys equal under ASCII case
//     folding (duplicate keys resolve differently across decoders);
//   - every header field is a string or null, and "transactions" is an array
//     or null with no float-overflow-prone numbers or deep nesting inside.
//
// Anything else takes the exact path: the original decoders, run as before.

// blockDoc is the fast-path split of a block response.
type blockDoc struct {
	members []member // top-level members in document order
	txs     []string // raw transaction entries (nil when absent/null)
}

type member struct{ key, val string }

// headerExact mirrors the original Header layout (transactions as []any),
// decoded with the original sonic call. It is the exact path.
type headerExact struct {
	Hash             string `json:"hash"`
	ParentHash       string `json:"parentHash"`
	StateRoot        string `json:"stateRoot"`
	TransactionsRoot string `json:"transactionsRoot"`
	ReceiptsRoot     string `json:"receiptsRoot"`
	LogsBloom        string `json:"logsBloom"`
	Number           string `json:"number"`
	Sha3Uncles       string `json:"sha3Uncles"`
	Difficulty       string `json:"difficulty"`
	Nonce            string `json:"nonce"`
	BlobGasUsed      string `json:"blobGasUsed"`
	Timestamp        string `json:"timestamp"`
	GasLimit         string `json:"gasLimit"`
	GasUsed          string `json:"gasUsed"`
	BaseFeePerGas    string `json:"baseFeePerGas"`
	RawTransactions  []any  `json:"transactions"`
}

func (e *headerExact) header() *Header {
	return &Header{
		Hash: e.Hash, ParentHash: e.ParentHash, StateRoot: e.StateRoot,
		TransactionsRoot: e.TransactionsRoot, ReceiptsRoot: e.ReceiptsRoot,
		LogsBloom: e.LogsBloom, Number: e.Number, Sha3Uncles: e.Sha3Uncles,
		Difficulty: e.Difficulty, Nonce: e.Nonce, BlobGasUsed: e.BlobGasUsed,
		Timestamp: e.Timestamp, GasLimit: e.GasLimit, GasUsed: e.GasUsed,
		BaseFeePerGas: e.BaseFeePerGas,
		Transactions:  TxList{anys: e.RawTransactions},
	}
}

// maxFastTxDepth bounds nesting inside the transactions array on the fast
// path. sonic's generic decoder stops at 4096 levels while encoding/json
// allows 10000; real transactions are 3-4 levels deep, so anything near
// either limit is left to the exact path.
const maxFastTxDepth = 64

// decodeBlockHeader decodes a block response's header. doc is non-nil when
// the fast path applied (see the file comment). Otherwise the header came
// from the exact sonic decode, and err is that decode's error.
func decodeBlockHeader(raw []byte) (*Header, *blockDoc, error) {
	if h, doc := decodeBlockFast(raw); h != nil {
		return h, doc, nil
	}
	var e headerExact
	if err := common.SonicCfg.Unmarshal(raw, &e); err != nil {
		return nil, nil, err
	}
	return e.header(), nil, nil
}

// UnmarshalJSON decodes a block object into the header view with the same
// decoder Decoded.Header uses, so a Header decoded anywhere else (e.g. the
// chain view's trusted header fetches) accepts and rejects exactly what the
// original []any-based struct decode did.
func (h *Header) UnmarshalJSON(b []byte) error {
	hh, _, err := decodeBlockHeader(b)
	if err != nil {
		return err
	}
	*h = *hh
	return nil
}

func decodeBlockFast(raw []byte) (*Header, *blockDoc) {
	if len(raw) == 0 || !isPlainJSON(raw) {
		return nil, nil
	}
	s := util.B2Str(raw)
	if i := skipSpace(s, 0); i >= len(s) || s[i] != '{' {
		return nil, nil
	}
	if !common.SonicCfg.Valid(raw) {
		return nil, nil
	}
	doc := &blockDoc{members: make([]member, 0, 32)}
	forEachPair(s, 0, func(k, v string) { doc.members = append(doc.members, member{k, v}) })

	h := &Header{}
	// Linear duplicate detection (foldedKeySet, the asciiFoldEq relation):
	// a hostile object with many members must not cost quadratic time.
	var seen foldedKeySet
	for _, m := range doc.members {
		if !seen.add(m.key) {
			return nil, nil // duplicate key
		}
		if asciiEqualFold(m.key, "transactions") {
			switch m.val[0] {
			case 'n': // null: absent list
			case '[':
				var st scanStats
				doc.txs = splitArray(m.val, &st)
				if st.riskyNums || st.maxDepth > maxFastTxDepth {
					return nil, nil
				}
				h.Transactions = TxList{raws: doc.txs}
			default:
				return nil, nil // type mismatch: let the exact decode report it
			}
			continue
		}
		dst := headerField(h, m.key)
		if dst == nil {
			continue // not a Header field: ignored by every decoder
		}
		switch m.val[0] {
		case '"':
			*dst = m.val[1 : len(m.val)-1]
		case 'n': // null leaves a string field untouched
		default:
			return nil, nil // type mismatch: let the exact decode report it
		}
	}
	return h, doc
}

// headerField maps a JSON key (ASCII case-insensitively, as both decoders do
// on plain input) to the Header string field it fills, or nil.
func headerField(h *Header, key string) *string {
	if len(key) < 4 || len(key) > 16 {
		return nil
	}
	for _, f := range [...]struct {
		name string
		dst  *string
	}{
		{"hash", &h.Hash}, {"parenthash", &h.ParentHash}, {"stateroot", &h.StateRoot},
		{"transactionsroot", &h.TransactionsRoot}, {"receiptsroot", &h.ReceiptsRoot},
		{"logsbloom", &h.LogsBloom}, {"number", &h.Number}, {"sha3uncles", &h.Sha3Uncles},
		{"difficulty", &h.Difficulty}, {"nonce", &h.Nonce}, {"blobgasused", &h.BlobGasUsed},
		{"timestamp", &h.Timestamp}, {"gaslimit", &h.GasLimit}, {"gasused", &h.GasUsed},
		{"basefeepergas", &h.BaseFeePerGas},
	} {
		if asciiEqualFold(key, f.name) {
			return f.dst
		}
	}
	return nil
}

// asciiFoldEq reports whether two plain keys are equal under ASCII case
// folding.
func asciiFoldEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// rebuildHeaderDoc returns the block object without its transactions, uncles
// and withdrawals bodies. The reference header decoder ignores those keys, so
// on a valid, duplicate-free document it decodes the rebuilt object exactly as
// it would the original, without scanning the bodies.
func (doc *blockDoc) rebuildHeaderDoc() []byte {
	n := 2
	for _, m := range doc.members {
		n += len(m.key) + len(m.val) + 4
	}
	buf := make([]byte, 0, n)
	buf = append(buf, '{')
	first := true
	for _, m := range doc.members {
		switch m.key {
		case "transactions", "uncles", "withdrawals":
			continue
		}
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = append(buf, m.key...)
		buf = append(buf, '"', ':')
		buf = append(buf, m.val...)
	}
	return append(buf, '}')
}

// splitTxs returns the per-transaction split of the shared fast-path doc
// (nil entries for non-object entries), computed once per response. Only
// valid when d.doc != nil.
func (d *Decoded) splitTxs() []*splitTx {
	if d.split != nil || d.doc == nil {
		return d.split
	}
	d.split = make([]*splitTx, len(d.doc.txs))
	for i, raw := range d.doc.txs {
		if len(raw) > 0 && raw[0] == '{' {
			d.split[i] = splitTxObject(raw)
		}
	}
	return d.split
}

// blockTxViews builds the typed Tx views for a block's full transaction
// objects. On the fast path each view comes from the same split the
// transactionsRoot recompute decodes, so a transaction is parsed once.
func (d *Decoded) blockTxViews(h *Header) []Tx {
	var out []Tx
	if d.doc != nil {
		for i, st := range d.splitTxs() {
			if st == nil {
				continue // hash-only entry; nothing to validate at tx level
			}
			if st.ok {
				if t, ok := st.txView(); ok {
					out = append(out, t)
				}
				continue
			}
			if obj, ok := h.Transactions.Object(i); ok {
				if t, ok := txViewFromObject(obj); ok {
					out = append(out, t)
				}
			}
		}
		return out
	}
	for i := 0; i < h.Transactions.Len(); i++ {
		obj, ok := h.Transactions.Object(i)
		if !ok {
			continue // hash-only entry; nothing to validate at tx level
		}
		if t, ok := txViewFromObject(obj); ok {
			out = append(out, t)
		}
	}
	return out
}

// txViewFromObject is the original view conversion: re-encode the generic
// object and decode it into Tx.
func txViewFromObject(obj map[string]any) (Tx, bool) {
	b, err := common.SonicCfg.Marshal(obj)
	if err != nil {
		return Tx{}, false
	}
	var t Tx
	if err := common.SonicCfg.Unmarshal(b, &t); err != nil {
		return Tx{}, false
	}
	return t, true
}
