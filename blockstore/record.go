package blockstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var errRecordTooLarge = errors.New("blockstore: block exceeds maxBlockBytes")

// rawBlock is the subset of block fields hydration validates.
type rawBlock struct {
	Number       string            `json:"number"`
	Hash         string            `json:"hash"`
	ParentHash   string            `json:"parentHash"`
	LogsBloom    string            `json:"logsBloom"`
	Transactions []json.RawMessage `json:"transactions"`

	// Cached txHashesOf result, computed once while the block is scanned.
	txDone bool
	txSet  map[string]struct{}
	txFull bool
	txErr  error
}

// rawLog is the subset of log fields validation and filtering use.
type rawLog struct {
	Address         string   `json:"address"`
	Topics          []string `json:"topics"`
	BlockNumber     string   `json:"blockNumber"`
	BlockHash       string   `json:"blockHash"`
	TransactionHash string   `json:"transactionHash"`
	LogIndex        string   `json:"logIndex"`
	Removed         bool     `json:"removed"`
}

func parseHexInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0, fmt.Errorf("not hex: %q", s)
	}
	return strconv.ParseInt(s[2:], 16, 64)
}

func normHash(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// parseBlockHeader extracts identity fields from a block result.
func parseBlockHeader(raw json.RawMessage) (*rawBlock, int64, error) {
	sb, n, err := parseScannedBlock(raw)
	if err != nil {
		return nil, 0, err
	}
	return sb.b, n, nil
}

// txHashesOf returns the transaction hashes of a full (or hash-only) block.
// A missing or null transactions field is rejected: only [] proves an empty block.
// A repeated hash is rejected too: a canonical block lists each transaction once,
// and callers compare these sets, so a duplicate would let a body with an extra
// (or missing) entry match a verified header.
// The result is computed once per parsed block and shared: callers must not
// modify the returned set.
func txHashesOf(b *rawBlock) (map[string]struct{}, bool, error) {
	if !b.txDone {
		b.computeTxHashes()
	}
	if b.txErr != nil {
		return nil, false, b.txErr
	}
	return b.txSet, b.txFull, nil
}

func validateCompleteLogs(b *rawBlock, n int64, txs map[string]struct{}, logsRaw json.RawMessage) error {
	var logs []rawLog
	trimmed := bytes.TrimSpace(logsRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("null logs")
	}
	if err := json.Unmarshal(trimmed, &logs); err != nil {
		return fmt.Errorf("logs: %w", err)
	}
	hash := normHash(b.Hash)
	var bloom types.Bloom
	prevIdx := int64(-1)
	for i := range logs {
		l := &logs[i]
		if normHash(l.BlockHash) != hash {
			return fmt.Errorf("log %d blockHash mismatch", i)
		}
		ln, err := parseHexInt(l.BlockNumber)
		if err != nil || ln != n {
			return fmt.Errorf("log %d blockNumber mismatch", i)
		}
		if _, ok := txs[normHash(l.TransactionHash)]; !ok {
			return fmt.Errorf("log %d transaction not in block", i)
		}
		if l.Removed {
			return fmt.Errorf("log %d marked removed", i)
		}
		idx, err := parseHexInt(l.LogIndex)
		if err != nil || idx <= prevIdx {
			return fmt.Errorf("log %d logIndex not increasing", i)
		}
		prevIdx = idx
		bloom.Add(common.HexToAddress(l.Address).Bytes())
		for _, topic := range l.Topics {
			bloom.Add(common.HexToHash(topic).Bytes())
		}
	}
	if b.LogsBloom == "" {
		return fmt.Errorf("block missing logsBloom")
	}
	if types.BytesToBloom(common.FromHex(b.LogsBloom)) != bloom {
		return fmt.Errorf("logsBloom mismatch (logs incomplete)")
	}
	return nil
}

// BlockJSON renders the block. full=false replaces transactions by hashes.
// Every other field is kept byte for byte as received.
func (r *BlockRecord) BlockJSON(full bool) (json.RawMessage, error) {
	if full {
		return r.Block, nil
	}
	if sb, err := scanBlock(r.Block); err == nil {
		if out, _, ok := sb.hashesOnly(); ok {
			return out, nil
		}
	}
	return legacyHashesOnly(r.Block)
}

// legacyHashesOnly is the map-based rendering, kept for inputs the splice
// does not cover (no or empty transactions, unvalidated transactions such as
// duplicates or elements without a hash), with its original results.
func legacyHashesOnly(block json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(block, &m); err != nil {
		return nil, err
	}
	var txs []json.RawMessage
	if err := json.Unmarshal(m["transactions"], &txs); err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(txs))
	for _, t := range txs {
		var tx struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(t, &tx); err != nil {
			return nil, err
		}
		hashes = append(hashes, tx.Hash)
	}
	hb, _ := json.Marshal(hashes)
	m["transactions"] = hb
	return json.Marshal(m)
}

// LogFilter is an eth_getLogs / eth_subscribe("logs") address+topics filter.
type LogFilter struct {
	Addresses map[string]struct{} // empty = any
	Topics    [][]string          // per position; nil/empty = wildcard
}

func isHexOfLen(s string, n int) bool {
	if len(s) != 2+n || (s[:2] != "0x" && s[:2] != "0X") {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func parseAddr(v interface{}) (string, error) {
	s, ok := v.(string)
	if !ok || !isHexOfLen(s, 40) {
		return "", fmt.Errorf("invalid address")
	}
	return normHash(s), nil
}

func parseTopic(v interface{}) (string, error) {
	s, ok := v.(string)
	if !ok || !isHexOfLen(s, 64) {
		return "", fmt.Errorf("invalid topic")
	}
	return normHash(s), nil
}

// ParseLogFilter parses the address/topics members of a filter object.
func ParseLogFilter(obj map[string]interface{}) (*LogFilter, error) {
	f := &LogFilter{}
	switch a := obj["address"].(type) {
	case nil:
	case string:
		s, err := parseAddr(a)
		if err != nil {
			return nil, err
		}
		f.Addresses = map[string]struct{}{s: {}}
	case []interface{}:
		f.Addresses = map[string]struct{}{}
		for _, v := range a {
			s, err := parseAddr(v)
			if err != nil {
				return nil, err
			}
			f.Addresses[s] = struct{}{}
		}
		if len(f.Addresses) == 0 {
			f.Addresses = nil
		}
	default:
		return nil, fmt.Errorf("invalid address")
	}
	switch t := obj["topics"].(type) {
	case nil:
	case []interface{}:
		if len(t) > 4 {
			return nil, fmt.Errorf("too many topics")
		}
		for _, pos := range t {
			switch p := pos.(type) {
			case nil:
				f.Topics = append(f.Topics, nil)
			case string:
				s, err := parseTopic(p)
				if err != nil {
					return nil, err
				}
				f.Topics = append(f.Topics, []string{s})
			case []interface{}:
				var alts []string
				wildcard := false
				for _, v := range p {
					if v == nil {
						wildcard = true
						continue
					}
					s, err := parseTopic(v)
					if err != nil {
						return nil, err
					}
					alts = append(alts, s)
				}
				if wildcard {
					alts = nil
				}
				f.Topics = append(f.Topics, alts)
			default:
				return nil, fmt.Errorf("invalid topic")
			}
		}
	default:
		return nil, fmt.Errorf("invalid topics")
	}
	return f, nil
}

func (f *LogFilter) match(l *rawLog) bool {
	// geth semantics: a filter with more topic positions than the log has
	// never matches, even when the extra positions are wildcards.
	if len(f.Topics) > len(l.Topics) {
		return false
	}
	if len(f.Addresses) > 0 {
		if _, ok := f.Addresses[normHash(l.Address)]; !ok {
			return false
		}
	}
	for i, alts := range f.Topics {
		if len(alts) == 0 {
			continue
		}
		if i >= len(l.Topics) {
			return false
		}
		t := normHash(l.Topics[i])
		ok := false
		for _, a := range alts {
			if a == t {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// FilterLogs returns the record's logs matching f, preserving upstream bytes.
// When removed is true each log is re-emitted with "removed":true.
func (r *BlockRecord) FilterLogs(f *LogFilter, removed bool) ([]json.RawMessage, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(r.Logs, &raws); err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(raws))
	for _, raw := range raws {
		var l rawLog
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, err
		}
		if f != nil && !f.match(&l) {
			continue
		}
		if removed {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			m["removed"] = json.RawMessage("true")
			b, _ := json.Marshal(m)
			raw = b
		}
		out = append(out, raw)
	}
	return out, nil
}
