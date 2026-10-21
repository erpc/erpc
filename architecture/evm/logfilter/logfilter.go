// Package logfilter is the one home of EVM log filter semantics: parsing a
// filter, matching a log (go-ethereum filterLogs), and the logs bloom test.
package logfilter

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/erpc/erpc/common"
	"golang.org/x/crypto/sha3"
)

// BloomLen is the byte length of a 2048-bit logs bloom.
const BloomLen = 256

// LogsFilter is a parsed eth_getLogs or eth_subscribe logs filter. Matching
// reproduces go-ethereum's filterLogs semantics:
//
//   - address: absent = any; a single address or an OR-list otherwise.
//   - topics: order-dependent, up to 4 positions; a nil/absent position is a
//     wildcard; a list at a position is an OR. A filter with MORE positions
//     than a log has topics does NOT match that log (even if the extra
//     positions are wildcards).
//   - fromBlock/toBlock: concrete hex bounds when both parse; block tags
//     (latest/pending/...) leave the range non-concrete (From/To = -1).
//   - blockHash: the single-block-by-hash variant (mutually exclusive with a
//     range, per spec; both are recorded and callers prefer BlockHash).
type LogsFilter struct {
	FromBlock int64 // -1 when absent or a non-concrete tag
	ToBlock   int64 // -1 when absent or a non-concrete tag
	BlockHash string
	addresses map[string]struct{} // empty = any; keys normalized via normHex
	topics    [][]string          // per position; nil/empty slice = wildcard; values normalized
	// Bloom bit positions of each address and of each topic value per
	// position; nil for a value that is not hex, which the bloom test
	// treats as present.
	addrBits  []*[3]uint16
	topicBits [][]*[3]uint16
	key       string
}

// normHex canonicalizes a hex string for set-membership comparison ("0X1A" ==
// "0x1a"). It does not strip leading zeros: addresses and topics are
// fixed-width, so byte-equality after lowercasing is the correct notion.
func normHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Parse parses the first param of an eth_getLogs request, or the filter
// param of an eth_subscribe logs request. It returns nil when the params
// are not a well-formed filter object.
func Parse(params []any) *LogsFilter {
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
				return nil
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

	f.index()
	return f
}

// ParseSubscribe parses the filter of an eth_subscribe logs request as geth
// does: absent or an object whose address is a 20-byte hex value or a list
// of them, and whose topics hold at most 4 positions, each null, a 32-byte
// hex value or a list of them. fromBlock and toBlock are ignored, as for a
// live geth subscription.
func ParseSubscribe(filter any) (*LogsFilter, error) {
	if filter == nil {
		filter = map[string]any{}
	}
	obj, ok := filter.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("logs filter must be an object")
	}
	switch a := obj["address"].(type) {
	case nil:
	case string:
		if !isHexBytes(a, 20) {
			return nil, fmt.Errorf("invalid address %q", a)
		}
	case []any:
		for _, v := range a {
			if s, ok := v.(string); !ok || !isHexBytes(s, 20) {
				return nil, fmt.Errorf("invalid address %v", v)
			}
		}
	default:
		return nil, fmt.Errorf("invalid address %v", a)
	}
	var topics []any
	switch ts := obj["topics"].(type) {
	case nil:
	case []any:
		if len(ts) > 4 {
			return nil, fmt.Errorf("too many topic positions: %d", len(ts))
		}
		topics = make([]any, len(ts))
		for i, pos := range ts {
			switch p := pos.(type) {
			case nil:
			case string:
				if !isHexBytes(p, 32) {
					return nil, fmt.Errorf("invalid topic %q", p)
				}
				topics[i] = p
			case []any:
				topics[i] = p
				for _, v := range p {
					if v == nil {
						// geth: a null in a position's list matches any topic.
						topics[i] = nil
						continue
					}
					if s, ok := v.(string); !ok || !isHexBytes(s, 32) {
						return nil, fmt.Errorf("invalid topic %v", v)
					}
				}
			default:
				return nil, fmt.Errorf("invalid topic %v", p)
			}
		}
	default:
		return nil, fmt.Errorf("invalid topics %v", ts)
	}
	live := map[string]any{"address": obj["address"]}
	if topics != nil {
		live["topics"] = topics
	}
	f := Parse([]any{live})
	if f == nil {
		return nil, fmt.Errorf("invalid logs filter")
	}
	return f, nil
}

// isHexBytes reports whether s is 0x-prefixed hex of exactly n bytes.
func isHexBytes(s string, n int) bool {
	if len(s) != 2+2*n || (s[:2] != "0x" && s[:2] != "0X") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

// index precomputes the bloom bits of every value and the filter's key.
func (f *LogsFilter) index() {
	addrs := make([]string, 0, len(f.addresses))
	for a := range f.addresses {
		addrs = append(addrs, a)
	}
	slices.Sort(addrs)
	var key strings.Builder
	key.WriteString("a:")
	for _, a := range addrs {
		f.addrBits = append(f.addrBits, bloomBitsHex(a))
		key.WriteString(a)
		key.WriteByte(',')
	}
	f.topicBits = make([][]*[3]uint16, len(f.topics))
	for i, sub := range f.topics {
		vals := slices.Clone(sub)
		slices.Sort(vals)
		key.WriteString("|t:")
		for _, v := range vals {
			f.topicBits[i] = append(f.topicBits[i], bloomBitsHex(v))
			key.WriteString(v)
			key.WriteByte(',')
		}
	}
	f.key = key.String()
}

// Key names the filter's address and topic criteria: two filters with one
// key match the same logs. Block range and hash are not part of it.
func (f *LogsFilter) Key() string { return f.key }

// Addresses are the filter's addresses, normalized; empty matches any.
func (f *LogsFilter) Addresses() []string {
	out := make([]string, 0, len(f.addresses))
	for a := range f.addresses {
		out = append(out, a)
	}
	return out
}

// Topic0 are the values of the filter's first topic position, normalized;
// empty is a wildcard.
func (f *LogsFilter) Topic0() []string {
	if len(f.topics) == 0 {
		return nil
	}
	return f.topics[0]
}

// ConcreteRange reports the [from, to] block range when the filter has one.
func (f *LogsFilter) ConcreteRange() (from, to int64, ok bool) {
	if f.FromBlock >= 0 && f.ToBlock >= f.FromBlock {
		return f.FromBlock, f.ToBlock, true
	}
	return 0, 0, false
}

// Matches reports whether a log with this address and these topics
// satisfies the filter's address and topics criteria (geth filterLogs).
// Block-range membership is the caller's concern.
func (f *LogsFilter) Matches(address string, topics []string) bool {
	if len(f.addresses) > 0 {
		if _, ok := f.addresses[normHex(address)]; !ok {
			return false
		}
	}
	// geth: a filter with more topic positions than the log has topics never
	// matches, regardless of wildcards.
	if len(f.topics) > len(topics) {
		return false
	}
	for i, sub := range f.topics {
		if len(sub) == 0 {
			continue
		}
		if !slices.Contains(sub, normHex(topics[i])) {
			return false
		}
	}
	return true
}

// MayMatch is the bloom test (geth bloomFilter): false only when bloom
// proves no log of its block matches the filter. A bloom that is not 256
// bytes proves nothing.
func (f *LogsFilter) MayMatch(bloom []byte) bool {
	if len(bloom) != BloomLen {
		return true
	}
	if len(f.addrBits) > 0 && !anyInBloom(bloom, f.addrBits) {
		return false
	}
	for _, sub := range f.topicBits {
		if len(sub) > 0 && !anyInBloom(bloom, sub) {
			return false
		}
	}
	return true
}

func anyInBloom(bloom []byte, values []*[3]uint16) bool {
	for _, bits := range values {
		if bits == nil || bloomHas(bloom, bits) {
			return true
		}
	}
	return false
}

func bloomHas(bloom []byte, bits *[3]uint16) bool {
	for _, b := range bits {
		if bloom[BloomLen-1-int(b>>3)]&(1<<(b&7)) == 0 {
			return false
		}
	}
	return true
}

func bloomBitsHex(s string) *[3]uint16 {
	b, err := common.HexToBytes(s)
	if err != nil {
		return nil
	}
	bits := bloomBits(b)
	return &bits
}

// bloomBits are the three keccak-derived bit positions of value in a
// 2048-bit bloom.
func bloomBits(value []byte) [3]uint16 {
	var h [32]byte
	hw := sha3.NewLegacyKeccak256()
	_, _ = hw.Write(value)
	hw.Sum(h[:0])
	var bits [3]uint16
	for i := range bits {
		bits[i] = (uint16(h[2*i])<<8 | uint16(h[2*i+1])) & 0x7FF
	}
	return bits
}

// BloomAdd sets value's three bit positions in a 256-byte, big-endian
// bloom.
func BloomAdd(bloom, value []byte) {
	for _, b := range bloomBits(value) {
		bloom[BloomLen-1-int(b>>3)] |= byte(1 << (b & 7))
	}
}
