package integrity

import (
	"encoding/binary"
	"strings"
)

// A minimal JSON splitter for documents that are ALREADY known to be valid
// JSON and to sit in the "plain" subset: printable ASCII, no backslash, no
// control bytes (see isPlainJSON). Inside that subset a string token is just
// `"` ... next `"` (no escapes can exist), keys are their raw bytes, and the
// only whitespace is the space character, so structural splitting needs no
// state machine. Every caller must establish both preconditions first; on
// anything else the shared decode falls back to the legacy decoders.

const (
	swarLo   = 0x0101010101010101
	swarHigh = 0x8080808080808080
)

// isPlainJSON reports whether b contains only bytes in [0x20, 0x7f] and no
// backslash. In that subset sonic and encoding/json agree on what the bytes
// mean (no escapes, no UTF-8 decoding, no control characters, no key folding
// beyond ASCII case), which is what lets the fast paths reproduce the stdlib
// decoders exactly.
func isPlainJSON(b []byte) bool {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		w := binary.LittleEndian.Uint64(b[i:])
		// high bit set: non-ASCII
		if w&swarHigh != 0 {
			return false
		}
		// any byte < 0x20
		if (w-swarLo*0x20)&^w&swarHigh != 0 {
			return false
		}
		// any byte == '\\'
		x := w ^ (swarLo * '\\')
		if (x-swarLo)&^x&swarHigh != 0 {
			return false
		}
	}
	for ; i < len(b); i++ {
		c := b[i]
		if c < 0x20 || c >= 0x80 || c == '\\' {
			return false
		}
	}
	return true
}

func skipSpace(s string, i int) int {
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return i
}

// scanStats records what the header's []any decode would have to accept
// inside a scanned value: nesting depth and any number token that could
// overflow a float64 (sonic rejects those; encoding/json accepts them).
type scanStats struct {
	maxDepth  int
	riskyNums bool
}

// valueEnd returns the index just past the value starting at s[i].
func valueEnd(s string, i int, st *scanStats) int {
	switch s[i] {
	case '"':
		return i + 1 + strings.IndexByte(s[i+1:], '"') + 1
	case '{', '[':
		depth := 0
		for j := i; j < len(s); j++ {
			switch c := s[j]; c {
			case '"':
				j += 1 + strings.IndexByte(s[j+1:], '"')
			case '{', '[':
				depth++
				if st != nil && depth > st.maxDepth {
					st.maxDepth = depth
				}
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				k := numberEnd(s, j)
				if st != nil && riskyNumber(s[j:k]) {
					st.riskyNums = true
				}
				j = k - 1
			}
		}
		return len(s)
	default:
		if st != nil && (s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
			k := numberEnd(s, i)
			if riskyNumber(s[i:k]) {
				st.riskyNums = true
			}
			return k
		}
		j := i
		for j < len(s) {
			switch s[j] {
			case ',', '}', ']', ' ':
				return j
			}
			j++
		}
		return j
	}
}

func numberEnd(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case '-', '+', '.', 'e', 'E', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			i++
		default:
			return i
		}
	}
	return i
}

// riskyNumber flags a number token sonic might refuse to decode into a
// float64 (exponent, or so many digits it could overflow). Conservative: a
// flag only routes the decode to the exact legacy path.
func riskyNumber(n string) bool {
	return len(n) > 300 || strings.ContainsAny(n, "eE")
}

// forEachPair calls fn for every member of the object at s[i:] in document
// order, duplicates included. Returns false if s[i] is not an object.
func forEachPair(s string, i int, fn func(key, val string)) bool {
	i = skipSpace(s, i)
	if i >= len(s) || s[i] != '{' {
		return false
	}
	i = skipSpace(s, i+1)
	if i < len(s) && s[i] == '}' {
		return true
	}
	for i < len(s) {
		kEnd := valueEnd(s, i, nil)
		key := s[i+1 : kEnd-1]
		i = skipSpace(s, kEnd)
		i = skipSpace(s, i+1) // ':'
		vEnd := valueEnd(s, i, nil)
		fn(key, s[i:vEnd])
		i = skipSpace(s, vEnd)
		if i >= len(s) || s[i] == '}' {
			return true
		}
		i = skipSpace(s, i+1) // ','
	}
	return true
}

// splitArray returns the raw elements of the array s (which must start with
// '['), recording depth/number stats for the whole array into st.
func splitArray(s string, st *scanStats) []string {
	i := skipSpace(s, 1)
	if i < len(s) && s[i] == ']' {
		st.maxDepth = max(st.maxDepth, 1)
		return []string{}
	}
	out := make([]string, 0, 64)
	for i < len(s) {
		var es scanStats
		end := valueEnd(s, i, &es)
		st.maxDepth = max(st.maxDepth, es.maxDepth+1)
		st.riskyNums = st.riskyNums || es.riskyNums
		out = append(out, s[i:end])
		i = skipSpace(s, end)
		if i >= len(s) || s[i] == ']' {
			break
		}
		i = skipSpace(s, i+1)
	}
	return out
}

// asciiEqualFold reports whether a equals the lowercase ASCII word b,
// ignoring ASCII case. Inside the plain subset this is exactly how both
// encoding/json (foldName) and sonic (exact, then strings.ToLower) match an
// object key to a struct field.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}
