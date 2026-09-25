package headcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// rawBlock is the subset of block fields hydration validates.
type rawBlock struct {
	Number       string            `json:"number"`
	Hash         string            `json:"hash"`
	ParentHash   string            `json:"parentHash"`
	LogsBloom    string            `json:"logsBloom"`
	Transactions []json.RawMessage `json:"transactions"`
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
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, 0, fmt.Errorf("null block")
	}
	var b rawBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, 0, err
	}
	n, err := parseHexInt(b.Number)
	if err != nil {
		return nil, 0, fmt.Errorf("block number: %w", err)
	}
	if b.Hash == "" || b.ParentHash == "" {
		return nil, 0, fmt.Errorf("block missing hash/parentHash")
	}
	return &b, n, nil
}

// txHashesOf returns the transaction hashes of a full (or hash-only) block.
func txHashesOf(b *rawBlock) (map[string]struct{}, bool, error) {
	out := make(map[string]struct{}, len(b.Transactions))
	full := true
	for _, t := range b.Transactions {
		t = bytes.TrimSpace(t)
		if len(t) > 0 && t[0] == '"' {
			full = false
			var h string
			if err := json.Unmarshal(t, &h); err != nil {
				return nil, false, err
			}
			out[normHash(h)] = struct{}{}
			continue
		}
		var tx struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(t, &tx); err != nil {
			return nil, false, err
		}
		if tx.Hash == "" {
			return nil, false, fmt.Errorf("transaction without hash")
		}
		out[normHash(tx.Hash)] = struct{}{}
	}
	return out, full, nil
}

// buildRecord validates a full block plus the complete log list for that block
// and returns an immutable record. Any inconsistency rejects the whole block:
// partial or mismatched data is never cached.
func buildRecord(blockRaw, logsRaw json.RawMessage, maxBlockBytes int64) (*BlockRecord, error) {
	if maxBlockBytes > 0 && int64(len(blockRaw)+len(logsRaw)) > maxBlockBytes {
		return nil, fmt.Errorf("block exceeds maxBlockBytes")
	}
	b, n, err := parseBlockHeader(blockRaw)
	if err != nil {
		return nil, err
	}
	txs, full, err := txHashesOf(b)
	if err != nil {
		return nil, err
	}
	if !full && len(b.Transactions) > 0 {
		return nil, fmt.Errorf("block fetched without full transactions")
	}
	var logs []rawLog
	trimmed := bytes.TrimSpace(logsRaw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("null logs")
	}
	if err := json.Unmarshal(trimmed, &logs); err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	hash := normHash(b.Hash)
	var bloom types.Bloom
	prevIdx := int64(-1)
	for i := range logs {
		l := &logs[i]
		if normHash(l.BlockHash) != hash {
			return nil, fmt.Errorf("log %d blockHash mismatch", i)
		}
		ln, err := parseHexInt(l.BlockNumber)
		if err != nil || ln != n {
			return nil, fmt.Errorf("log %d blockNumber mismatch", i)
		}
		if _, ok := txs[normHash(l.TransactionHash)]; !ok {
			return nil, fmt.Errorf("log %d transaction not in block", i)
		}
		if l.Removed {
			return nil, fmt.Errorf("log %d marked removed", i)
		}
		idx, err := parseHexInt(l.LogIndex)
		if err != nil || idx <= prevIdx {
			return nil, fmt.Errorf("log %d logIndex not increasing", i)
		}
		prevIdx = idx
		bloom.Add(common.HexToAddress(l.Address).Bytes())
		for _, t := range l.Topics {
			bloom.Add(common.HexToHash(t).Bytes())
		}
	}
	// The logs bloom commits to every (address, topic) of the block's logs. A
	// mismatch means the log list is incomplete or foreign (e.g. an upstream
	// returning [] for a block it has not indexed yet).
	if b.LogsBloom == "" {
		return nil, fmt.Errorf("block missing logsBloom")
	}
	want := types.BytesToBloom(common.FromHex(b.LogsBloom))
	if want != bloom {
		return nil, fmt.Errorf("logsBloom mismatch (logs incomplete)")
	}
	return &BlockRecord{
		Number:     n,
		Hash:       hash,
		ParentHash: normHash(b.ParentHash),
		Block:      append(json.RawMessage(nil), blockRaw...),
		Logs:       append(json.RawMessage(nil), trimmed...),
	}, nil
}

// BlockJSON renders the block. full=false replaces transactions by hashes.
func (r *BlockRecord) BlockJSON(full bool) (json.RawMessage, error) {
	if full {
		return r.Block, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Block, &m); err != nil {
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

// HeaderJSON renders the block as a newHeads notification payload (header
// fields only).
func (r *BlockRecord) HeaderJSON() (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Block, &m); err != nil {
		return nil, err
	}
	for _, k := range []string{"transactions", "uncles", "size", "totalDifficulty", "withdrawals"} {
		delete(m, k)
	}
	return json.Marshal(m)
}

// LogFilter is an eth_getLogs / eth_subscribe("logs") address+topics filter.
type LogFilter struct {
	Addresses map[string]struct{} // empty = any
	Topics    [][]string          // per position; nil/empty = wildcard
}

// ParseLogFilter parses the address/topics members of a filter object.
func ParseLogFilter(obj map[string]interface{}) (*LogFilter, error) {
	f := &LogFilter{}
	switch a := obj["address"].(type) {
	case nil:
	case string:
		f.Addresses = map[string]struct{}{normHash(a): {}}
	case []interface{}:
		f.Addresses = map[string]struct{}{}
		for _, v := range a {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("invalid address")
			}
			f.Addresses[normHash(s)] = struct{}{}
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
				f.Topics = append(f.Topics, []string{normHash(p)})
			case []interface{}:
				var alts []string
				for _, v := range p {
					if v == nil {
						alts = nil
						break
					}
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("invalid topic")
					}
					alts = append(alts, normHash(s))
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
