package blockstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unsafe"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
)

// Single-scan block parsing. A block result is decoded once with sonic: the
// identity fields are copied out and each transactions element is kept as a
// no-copy slice of the input. Each element's hash is then located without
// materializing the transaction (sonic's searcher stops at the "hash" key),
// and the hashes-only form is built by splicing a hashes array over the
// transactions value, keeping every other byte as received.
//
// Validation matches the encoding/json implementation it replaces: any
// element the fast path cannot handle (not an object or plain string, no
// exact "hash" key, escapes) is decoded the old way, with the old errors.

// scanCfg decodes like encoding/json: case-insensitive keys, control
// characters in strings rejected, skipped values validated, strings copied
// (identity fields outlive the possibly large input).
var scanCfg = sonic.Config{CopyString: true, ValidateString: true}.Froze()

type blockScan struct {
	Number       string                   `json:"number"`
	Hash         string                   `json:"hash"`
	ParentHash   string                   `json:"parentHash"`
	LogsBloom    string                   `json:"logsBloom"`
	Transactions []sonic.NoCopyRawMessage `json:"transactions"`
}

// scannedBlock is one decoded block result plus its transaction hashes.
type scannedBlock struct {
	raw json.RawMessage
	b   *rawBlock
	// hashes are the transaction hashes as received (not normalized), in
	// order; nil when b.txErr is set.
	hashes []string
}

func bytesString(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// scanBlock decodes raw once. It does not validate identity fields.
func scanBlock(raw json.RawMessage) (*scannedBlock, error) {
	var s blockScan
	if err := scanCfg.UnmarshalFromString(bytesString(raw), &s); err != nil {
		return nil, err
	}
	b := &rawBlock{Number: s.Number, Hash: s.Hash, ParentHash: s.ParentHash, LogsBloom: s.LogsBloom}
	if s.Transactions != nil {
		b.Transactions = make([]json.RawMessage, len(s.Transactions))
		for i, t := range s.Transactions {
			b.Transactions[i] = json.RawMessage(t)
		}
	}
	sb := &scannedBlock{raw: raw, b: b}
	sb.hashes = b.computeTxHashes()
	return sb, nil
}

// computeTxHashes fills b's cached txHashesOf result and returns the hashes
// as received, in order (nil on error).
func (b *rawBlock) computeTxHashes() []string {
	b.txDone = true
	if b.Transactions == nil {
		b.txErr = fmt.Errorf("block missing transactions array")
		return nil
	}
	set := make(map[string]struct{}, len(b.Transactions))
	hashes := make([]string, 0, len(b.Transactions))
	full := true
	for _, t := range b.Transactions {
		h, isHash, err := elemTxHash(t)
		if err != nil {
			b.txErr = err
			return nil
		}
		if isHash {
			full = false
		}
		n := strings.Clone(normHash(h))
		if _, dup := set[n]; dup {
			b.txErr = fmt.Errorf("duplicate transaction %s", n)
			return nil
		}
		set[n] = struct{}{}
		hashes = append(hashes, h)
	}
	b.txSet, b.txFull = set, full
	return hashes
}

// elemTxHash returns one transactions element's hash and whether the element
// was a bare hash string (a hashes-only block) rather than an object.
func elemTxHash(t json.RawMessage) (string, bool, error) {
	t = bytes.TrimSpace(t)
	if len(t) >= 2 && t[0] == '"' {
		if t[len(t)-1] == '"' && plainASCII(t[1:len(t)-1]) {
			return string(t[1 : len(t)-1]), true, nil
		}
		var h string
		if err := json.Unmarshal(t, &h); err != nil {
			return "", true, err
		}
		return h, true, nil
	}
	if len(t) > 0 && t[0] == '{' && bytes.IndexByte(t, '\\') < 0 && hashKeys(t) == 1 {
		// One key equal (case-insensitively) to "hash" and no escapes: the
		// exact "hash" member, if present, is what encoding/json decodes.
		s := ast.NewSearcher(bytesString(t))
		// Already validated by the block decode.
		s.ValidateJSON = false
		if n, err := s.GetByPath("hash"); err == nil {
			if h, err := n.StrictString(); err == nil && h != "" && plainASCII([]byte(h)) {
				return strings.Clone(h), false, nil
			}
		}
	}
	// Anything else (null, numbers, case-variant or escaped keys, non-string
	// hashes) takes the encoding/json path for identical results and errors.
	var tx struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(t, &tx); err != nil {
		return "", false, err
	}
	if tx.Hash == "" {
		return "", false, fmt.Errorf("transaction without hash")
	}
	return tx.Hash, false, nil
}

// plainASCII reports whether s is printable ASCII without quotes or escapes,
// so its JSON string form decodes to exactly these bytes.
func plainASCII(s []byte) bool {
	for _, c := range s {
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// hashKeys counts quoted tokens in t that equal "hash" case-insensitively
// (keys or values; with no escapes in t every quote delimits a string).
func hashKeys(t []byte) int {
	count := 0
	for i := 0; i < len(t); {
		j := bytes.IndexByte(t[i:], '"')
		if j < 0 {
			break
		}
		j += i
		k := bytes.IndexByte(t[j+1:], '"')
		if k < 0 {
			break
		}
		k += j + 1
		if k-j-1 == 4 && bytes.EqualFold(t[j+1:k], []byte("hash")) {
			count++
		}
		i = k + 1
	}
	return count
}

// txArraySpan returns the byte range of the transactions array value in
// raw, located from the no-copy elements' positions (sonic's elements are
// slices of the input). ok is false when the elements do not point into raw
// (e.g. a decoder that copies) or the brackets are not where expected.
func (sb *scannedBlock) txArraySpan() (start, end int, ok bool) {
	txs := sb.b.Transactions
	if len(txs) == 0 || len(sb.raw) == 0 {
		return 0, 0, false
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(sb.raw)))
	off := func(e []byte) (int, bool) {
		if len(e) == 0 {
			return 0, false
		}
		p := uintptr(unsafe.Pointer(unsafe.SliceData(e)))
		if p < base || p-base+uintptr(len(e)) > uintptr(len(sb.raw)) {
			return 0, false
		}
		return int(p - base), true
	}
	first, ok1 := off(txs[0])
	last, ok2 := off(txs[len(txs)-1])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	start = first - 1
	for start >= 0 && isJSONSpace(sb.raw[start]) {
		start--
	}
	end = last + len(txs[len(txs)-1])
	for end < len(sb.raw) && isJSONSpace(sb.raw[end]) {
		end++
	}
	if start < 0 || end >= len(sb.raw) || sb.raw[start] != '[' || sb.raw[end] != ']' {
		return 0, 0, false
	}
	// The array must be the value of the exact key "transactions" (the
	// decoder matches keys case-insensitively; the rendering it replaces
	// looked up the exact key).
	k := start - 1
	for k >= 0 && isJSONSpace(sb.raw[k]) {
		k--
	}
	if k < 0 || sb.raw[k] != ':' {
		return 0, 0, false
	}
	k--
	for k >= 0 && isJSONSpace(sb.raw[k]) {
		k--
	}
	const key = `"transactions"`
	if k+1 < len(key) || string(sb.raw[k+1-len(key):k+1]) != key {
		return 0, 0, false
	}
	return start, end + 1, true
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// hashesOnly renders a full block as eth_getBlockByNumber(n, false) by
// replacing the transactions array with its hashes; every other byte is kept
// as received. The returned header has the same identity and transaction set.
func (sb *scannedBlock) hashesOnly() (json.RawMessage, *rawBlock, bool) {
	if sb.hashes == nil || !sb.b.txFull || bytes.Count(sb.raw, []byte(`"transactions"`)) != 1 {
		// Repeated keys keep the legacy rendering's exact semantics.
		return nil, nil, false
	}
	start, end, ok := sb.txArraySpan()
	if !ok {
		return nil, nil, false
	}
	size := len(sb.raw) - (end - start) + 2
	for _, h := range sb.hashes {
		size += len(h) + 3
	}
	out := make([]byte, 0, size)
	out = append(out, sb.raw[:start]...)
	out = append(out, '[')
	offs := make([][2]int, len(sb.hashes))
	for i, h := range sb.hashes {
		if i > 0 {
			out = append(out, ',')
		}
		at := len(out)
		if needsEscape(h) {
			q, _ := json.Marshal(h)
			out = append(out, q...)
		} else {
			out = append(out, '"')
			out = append(out, h...)
			out = append(out, '"')
		}
		offs[i] = [2]int{at, len(out)}
	}
	out = append(out, ']')
	out = append(out, sb.raw[end:]...)
	b := *sb.b
	b.Transactions = make([]json.RawMessage, len(offs))
	for i, o := range offs {
		b.Transactions[i] = json.RawMessage(out[o[0]:o[1]:o[1]])
	}
	b.txFull = false
	return out, &b, true
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c >= 0x7f {
			return true
		}
	}
	return false
}

// validateIdentity applies parseBlockHeader's checks to a scanned block.
func (b *rawBlock) validateIdentity() (int64, error) {
	n, err := parseHexInt(b.Number)
	if err != nil {
		return 0, fmt.Errorf("block number: %w", err)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative block number")
	}
	if !isHexOfLen(b.Hash, 64) || !isHexOfLen(b.ParentHash, 64) {
		return 0, fmt.Errorf("block has invalid hash/parentHash")
	}
	return n, nil
}

func isNullBlock(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// parseScannedBlock is parseBlockHeader keeping the scan for reuse.
func parseScannedBlock(raw json.RawMessage) (*scannedBlock, int64, error) {
	if isNullBlock(raw) {
		return nil, 0, fmt.Errorf("null block")
	}
	sb, err := scanBlock(raw)
	if err != nil {
		return nil, 0, err
	}
	n, err := sb.b.validateIdentity()
	if err != nil {
		return nil, 0, err
	}
	return sb, n, nil
}
