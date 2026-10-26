package evm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
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

	// confirmedAt and logsAt are when the header, as a pin, first stood at
	// or below the confirmed head and the logs head (Block.Ready), in unix
	// nanoseconds; 0 until then, and again once a revert removes the pin.
	// The tracker writes them before it publishes the state that holds
	// them there; a Read that returns the block first sets logsAt (ready).
	confirmedAt, logsAt atomic.Int64

	// encodings holds each transport's encoding of the header (a
	// *HeaderEncoding key), so it is dropped with the header.
	encodings sync.Map
}

var errMalformedHeader = errors.New("malformed block header")

// parseChainHeader is intake check 0: number, hash and parentHash must parse
// into fixed types; timestamp, logsBloom, transactionsRoot and gasUsed that
// do not parse count as absent.
func parseChainHeader(raw json.RawMessage) (*chainHeader, error) {
	h, ts, err := decodeChainHeader(raw)
	if err != nil {
		return nil, err
	}
	return checkChainHeader(raw, h, ts)
}

// headerJSON decodes the optional header fields raw, so one in an encoding
// integrity.Header does not take (a JSON integer timestamp) cannot fail the
// whole header.
type headerJSON struct {
	integrity.Header
	Timestamp        json.RawMessage `json:"timestamp"`
	LogsBloom        json.RawMessage `json:"logsBloom"`
	TransactionsRoot json.RawMessage `json:"transactionsRoot"`
	GasUsed          json.RawMessage `json:"gasUsed"`
}

// decodeChainHeader decodes raw once for intake check 0. The optional
// fields that parse are set on h in the form integrity reads (timestamp as
// hex); the others stay empty, as when absent. ts is the timestamp, 0 when
// absent.
func decodeChainHeader(raw json.RawMessage) (h *integrity.Header, ts uint64, err error) {
	var j headerJSON
	if err := common.SonicCfg.Unmarshal(raw, &j); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", errMalformedHeader, err)
	}
	h = &j.Header
	if t, ok := jsonTimestamp(j.Timestamp); ok {
		ts = t
		h.Timestamp = "0x" + strconv.FormatUint(t, 16)
	}
	if s, ok := jsonString(j.LogsBloom); ok && isHexBytes(s, 256) {
		h.LogsBloom = s
	}
	if s, ok := jsonString(j.TransactionsRoot); ok && isHexBytes(s, 32) {
		h.TransactionsRoot = s
	}
	if s, ok := jsonString(j.GasUsed); ok {
		if gas, err := common.HexToInt64(s); err == nil && gas >= 0 {
			h.GasUsed = s
		}
	}
	return h, ts, nil
}

// jsonTimestamp reads a timestamp as main does: a hex or decimal string,
// or a JSON integer.
func jsonTimestamp(raw json.RawMessage) (uint64, bool) {
	s, ok := jsonString(raw)
	if !ok {
		s = string(raw)
	}
	ts, err := integrity.ParseTimestamp(s)
	if err != nil || ts < 0 {
		return 0, false
	}
	return uint64(ts), true
}

// jsonString is raw's value when it is a JSON string without escapes; no
// hex field needs one.
func jsonString(raw json.RawMessage) (string, bool) {
	n := len(raw)
	if n < 2 || raw[0] != '"' || raw[n-1] != '"' || bytes.IndexByte(raw[1:n-1], '\\') >= 0 {
		return "", false
	}
	return string(raw[1 : n-1]), true
}

// checkChainHeader is intake check 0 over raw already decoded into h with
// timestamp ts (decodeChainHeader).
func checkChainHeader(raw json.RawMessage, h *integrity.Header, ts uint64) (*chainHeader, error) {
	n, err := common.HexToInt64(h.Number)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%w: number %q", errMalformedHeader, h.Number)
	}
	ch := &chainHeader{number: uint64(n), timestamp: ts, raw: raw, parsed: h}
	if !decodeHex32(h.Hash, &ch.hash) {
		return nil, fmt.Errorf("%w: hash %q", errMalformedHeader, h.Hash)
	}
	if !decodeHex32(h.ParentHash, &ch.parentHash) {
		return nil, fmt.Errorf("%w: parentHash %q", errMalformedHeader, h.ParentHash)
	}
	if h.LogsBloom != "" {
		ch.bloom, _ = hex.DecodeString(h.LogsBloom[2:])
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

// Parent is the header's parentHash.
func (s SourceHeader) Parent() [32]byte { return s.h.parentHash }

// Timestamp is the header's timestamp in seconds, 0 when it carries none.
func (s SourceHeader) Timestamp() int64 { return int64(s.h.timestamp) }

// Raw is the header as its source sent it.
func (s SourceHeader) Raw() json.RawMessage { return s.h.raw }

func (h *chainHeader) cursor() indexer.Cursor {
	return indexer.Cursor{Number: h.number, Hash: h.hash}
}

func (h *chainHeader) block() indexer.Block {
	return indexer.Block{Number: h.number, Hash: h.hash, ParentHash: h.parentHash, Header: h.raw}
}

// ready is when h became deliverable to a Read: when it reached the logs
// head for a Read with receipts, else the confirmed head. A Read can
// return a block before the tracker stamps it: its receipts held before
// the logs head moves, or a pin a revert unstamped after the Read loaded
// its state; then it is deliverable now, and a logs Read stamps that so
// every logs tap measures from it.
func (h *chainHeader) ready(logs bool) time.Time {
	if !logs {
		if at := h.confirmedAt.Load(); at != 0 {
			return time.Unix(0, at)
		}
		return time.Now()
	}
	at := h.logsAt.Load()
	if at == 0 {
		at = time.Now().UnixNano()
		if !h.logsAt.CompareAndSwap(0, at) {
			if l := h.logsAt.Load(); l != 0 {
				at = l
			}
		}
	}
	return time.Unix(0, at)
}

// logsReady is ready(true) for a logs Read that needs none of h's
// receipts: the logs-head stamp, or now while there is none, never set by
// that Read, since a tap that needs the receipts may still wait on them.
func (h *chainHeader) logsReady() time.Time {
	if at := h.logsAt.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Now()
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

// StreamBlockHeader is the gRPC StreamBlocks header of a block: its proto
// header, transactions and withdrawals left out.
func StreamBlockHeader(block json.RawMessage) (*bdsevm.BlockHeader, error) {
	var b bdsevm.JsonRpcBlock
	if err := sonic.Unmarshal(block, &b); err != nil {
		return nil, err
	}
	b.Transactions, b.Withdrawals = nil, nil
	pb, err := b.ToProto()
	if err != nil {
		return nil, err
	}
	return pb.Header, nil
}
