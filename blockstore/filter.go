package blockstore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// rawLog is the subset of log fields splitting and filtering use.
type rawLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	BlockNumber string   `json:"blockNumber"`
	BlockHash   string   `json:"blockHash"`
	LogIndex    string   `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

func parseHexInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0, fmt.Errorf("not hex: %q", s)
	}
	return strconv.ParseInt(s[2:], 16, 64)
}

func normHash(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// LogFilter is an eth_getLogs address+topics filter.
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

// filterLogs returns the logs of one raw JSON log list matching f (nil f
// matches all), preserving upstream bytes. The result is never nil.
func filterLogs(logsRaw json.RawMessage, f *LogFilter) ([]json.RawMessage, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(logsRaw, &raws); err != nil {
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
		out = append(out, raw)
	}
	return out, nil
}
