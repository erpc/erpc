package integrity

import (
	"strings"

	"github.com/erpc/erpc/common"
)

// LogsFilter is the parsed eth_getLogs request filter. Matching reproduces
// go-ethereum's filterLogs semantics exactly (the canonical reference — erpc
// itself never filters logs, it only splits ranges):
//
//   - address: absent = any; a single address or an OR-list otherwise.
//   - topics: order-dependent, up to 4 positions; a nil/absent position is a
//     wildcard; a list at a position is an OR. A filter with MORE positions
//     than a log has topics does NOT match that log (even if the extra
//     positions are wildcards) — geth behavior.
//   - fromBlock/toBlock: concrete hex bounds when both parse; block tags
//     (latest/pending/...) leave the range non-concrete (From/To = -1).
//   - blockHash: the single-block-by-hash variant (mutually exclusive with a
//     range, per spec; we just record both and let callers prefer BlockHash).
type LogsFilter struct {
	FromBlock int64 // -1 when absent or a non-concrete tag
	ToBlock   int64 // -1 when absent or a non-concrete tag
	BlockHash string
	addresses map[string]struct{} // empty = any; keys normalized via normHex
	topics    [][]string          // per position; nil/empty slice = wildcard; values normalized
}

// normHex canonicalizes a hex string for set-membership comparison ("0X1A" ==
// "0x1a"). It deliberately does NOT strip leading zeros — addresses and topics
// are fixed-width, so byte-equality after lowercasing is the correct notion.
func normHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// parseLogsFilter parses the first param of an eth_getLogs request. Returns nil
// when the params don't look like a well-formed filter object — callers must
// treat nil as "cannot reproduce the filter semantics, skip" (never reject on a
// filter we couldn't parse).
func parseLogsFilter(params []any) *LogsFilter {
	if len(params) < 1 {
		return nil
	}
	obj, ok := params[0].(map[string]any)
	if !ok {
		return nil
	}
	f := &LogsFilter{FromBlock: -1, ToBlock: -1, addresses: map[string]struct{}{}}

	if bh, ok := obj["blockHash"].(string); ok && bh != "" {
		f.BlockHash = normHex(bh)
	}
	if fb, ok := obj["fromBlock"].(string); ok {
		if n, err := common.HexToInt64(fb); err == nil && n >= 0 {
			f.FromBlock = n
		}
	}
	if tb, ok := obj["toBlock"].(string); ok {
		if n, err := common.HexToInt64(tb); err == nil && n >= 0 {
			f.ToBlock = n
		}
	}
	// A half-concrete range (e.g. toBlock: "latest") is non-concrete as a range.
	if f.FromBlock < 0 || f.ToBlock < 0 || f.ToBlock < f.FromBlock {
		f.FromBlock, f.ToBlock = -1, -1
	}

	switch a := obj["address"].(type) {
	case nil:
	case string:
		if a != "" {
			f.addresses[normHex(a)] = struct{}{}
		}
	case []any:
		for _, v := range a {
			s, ok := v.(string)
			if !ok {
				return nil // non-string address entry — can't reproduce semantics
			}
			if s != "" {
				f.addresses[normHex(s)] = struct{}{}
			}
		}
	default:
		return nil
	}

	switch ts := obj["topics"].(type) {
	case nil:
	case []any:
		if len(ts) > 4 {
			return nil
		}
		f.topics = make([][]string, len(ts))
		for i, pos := range ts {
			switch p := pos.(type) {
			case nil: // wildcard position
			case string:
				f.topics[i] = []string{normHex(p)}
			case []any:
				for _, v := range p {
					s, ok := v.(string)
					if !ok {
						return nil
					}
					f.topics[i] = append(f.topics[i], normHex(s))
				}
			default:
				return nil
			}
		}
	default:
		return nil
	}

	return f
}

// ConcreteRange reports the [from, to] block range when the filter has one.
func (f *LogsFilter) ConcreteRange() (from, to int64, ok bool) {
	if f.FromBlock >= 0 && f.ToBlock >= f.FromBlock {
		return f.FromBlock, f.ToBlock, true
	}
	return 0, 0, false
}

// Matches reports whether a log satisfies the filter's address+topics criteria
// (geth filterLogs semantics). Block-range membership is the caller's concern.
func (f *LogsFilter) Matches(l *Log) bool {
	if len(f.addresses) > 0 {
		if _, ok := f.addresses[normHex(l.Address)]; !ok {
			return false
		}
	}
	// geth: a filter with more topic positions than the log has topics never
	// matches, regardless of wildcards.
	if len(f.topics) > len(l.Topics) {
		return false
	}
	for i, sub := range f.topics {
		if len(sub) == 0 {
			continue // wildcard position
		}
		match := false
		lt := normHex(l.Topics[i])
		for _, want := range sub {
			if lt == want {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}
