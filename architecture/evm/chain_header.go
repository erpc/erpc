package evm

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
)

// chainHeader is one held header: its fixed fields, its bytes as first held,
// and the integrity view of it.
type chainHeader struct {
	number     uint64
	hash       [32]byte
	parentHash [32]byte
	timestamp  uint64 // 0 when the header carries none
	hashHex    string // lowercase, 0x-prefixed
	raw        json.RawMessage
	parsed     *integrity.Header
	bloom      []byte // logsBloom; nil when the header carries none
	// emptyTxs: transactionsRoot is the empty-trie root, so the block has
	// no transactions and no receipts.
	emptyTxs bool

	recompute  sync.Once
	recomputes bool // the header hashes to its hash (blockHashRecompute)

	// intakePassed: this header passed intake's header checks, so a source
	// that sends the same hash is not validated again.
	intakePassed atomic.Bool
}

var errMalformedHeader = errors.New("malformed block header")

// parseChainHeader is intake check 0: number, hash and parentHash must parse
// into fixed types; timestamp, logsBloom and transactionsRoot must too when
// present.
func parseChainHeader(raw json.RawMessage) (*chainHeader, error) {
	var h integrity.Header
	if err := common.SonicCfg.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("%w: %v", errMalformedHeader, err)
	}
	n, err := common.HexToInt64(h.Number)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%w: number %q", errMalformedHeader, h.Number)
	}
	ch := &chainHeader{number: uint64(n), raw: raw, parsed: &h}
	if !decodeHex32(h.Hash, &ch.hash) {
		return nil, fmt.Errorf("%w: hash %q", errMalformedHeader, h.Hash)
	}
	if !decodeHex32(h.ParentHash, &ch.parentHash) {
		return nil, fmt.Errorf("%w: parentHash %q", errMalformedHeader, h.ParentHash)
	}
	if h.Timestamp != "" {
		ts, err := common.HexToInt64(h.Timestamp)
		if err != nil || ts < 0 {
			return nil, fmt.Errorf("%w: timestamp %q", errMalformedHeader, h.Timestamp)
		}
		ch.timestamp = uint64(ts)
	}
	if h.LogsBloom != "" {
		if !isHexBytes(h.LogsBloom, 256) {
			return nil, fmt.Errorf("%w: logsBloom", errMalformedHeader)
		}
		ch.bloom, _ = hex.DecodeString(h.LogsBloom[2:])
	}
	if h.TransactionsRoot != "" && !isHexBytes(h.TransactionsRoot, 32) {
		return nil, fmt.Errorf("%w: transactionsRoot %q", errMalformedHeader, h.TransactionsRoot)
	}
	ch.emptyTxs = strings.EqualFold(h.TransactionsRoot, integrity.EmptyTrieRoot)
	ch.hashHex = hexHash(ch.hash)
	return ch, nil
}

// ParseHeader is intake check 0 for a source header; it returns the header's
// number and hash.
func ParseHeader(raw json.RawMessage) (indexer.Cursor, error) {
	h, err := parseChainHeader(raw)
	if err != nil {
		return indexer.Cursor{}, err
	}
	return h.cursor(), nil
}

// SourceHeader is a source header past intake check 0, parsed once for the
// later checks and every tracker it votes in.
type SourceHeader struct{ h *chainHeader }

// ParseSourceHeader is intake check 0 for a source header.
func ParseSourceHeader(raw json.RawMessage) (SourceHeader, error) {
	h, err := parseChainHeader(raw)
	if err != nil {
		return SourceHeader{}, err
	}
	return SourceHeader{h}, nil
}

func (s SourceHeader) Cursor() indexer.Cursor { return s.h.cursor() }

func (h *chainHeader) cursor() indexer.Cursor {
	return indexer.Cursor{Number: h.number, Hash: h.hash}
}

func (h *chainHeader) block() indexer.Block {
	return indexer.Block{Number: h.number, Hash: h.hash, ParentHash: h.parentHash, Header: h.raw}
}

// hashRecomputes reports whether the header's JSON hashes to its hash
// (blockHashRecompute); computed once, on first use.
func (h *chainHeader) hashRecomputes() bool {
	h.recompute.Do(func() {
		got, ok := integrity.RecomputeBlockHash(h.raw)
		h.recomputes = ok && strings.EqualFold(got, h.hashHex)
	})
	return h.recomputes
}

func trimHexPrefix(s string) (string, bool) {
	if len(s) < 2 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return "", false
	}
	return s[2:], true
}

func decodeHex32(s string, dst *[32]byte) bool {
	digits, ok := trimHexPrefix(s)
	if !ok || len(digits) != 64 {
		return false
	}
	_, err := hex.Decode(dst[:], []byte(digits))
	return err == nil
}

func isHexBytes(s string, n int) bool {
	digits, ok := trimHexPrefix(s)
	if !ok || len(digits) != 2*n {
		return false
	}
	for i := range len(digits) {
		c := digits[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// hashKey normalizes a hex hash for lookups: hashes compare as bytes, so
// 0xAB… and 0xab… are one block.
func hashKey(s string) (string, bool) {
	var h [32]byte
	if !decodeHex32(s, &h) {
		return "", false
	}
	return hexHash(h), true
}

// hexHash is the lowercase 0x-prefixed form held headers are keyed by.
func hexHash(h [32]byte) string {
	var buf [66]byte
	buf[0], buf[1] = '0', 'x'
	hex.Encode(buf[2:], h[:])
	return string(buf[:])
}

var newHeadsDropped = []string{"transactions", "uncles", "withdrawals", "size"}

// NewHeadsPayload is the eth_subscribe newHeads payload of a block: its JSON
// without transactions, uncles, withdrawals and size, other fields in order.
func NewHeadsPayload(block json.RawMessage) (json.RawMessage, error) {
	root, err := sonic.Get(block)
	if err != nil {
		return nil, err
	}
	for _, key := range newHeadsDropped {
		if _, err := root.Unset(key); err != nil {
			return nil, err
		}
	}
	return root.MarshalJSON()
}
