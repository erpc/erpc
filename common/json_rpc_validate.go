package common

import (
	"fmt"

	"github.com/bytedance/sonic/ast"
)

// JSON-RPC 2.0 framing checks applied at the edge, before a request is allowed
// to touch auth, rate limiting, network bootstrap or any metric.
//
// Method names are an open set — eRPC never enumerates them, and must not.
// What today's traffic does force is a claim about their SHAPE: every method
// any chain or vendor has ever exposed is a short ASCII token
// (`eth_call`, `getAccountInfo`, `rpc.discover`, `zks_getBridgeContracts`).
// Everything eRPC does downstream already treats the method as such a token:
// it becomes a Prometheus label (`category` on ~40 metric families), a
// cache-key component, a span attribute and a config-matcher subject.
//
// Scanner traffic violates that shape — SQL/script payloads glued onto a real
// method name (`eth_call0' OR 157=(SELECT 157 FROM PG_SLEEP(15))--`). Each
// distinct payload used to mint a permanent Prometheus series per metric
// family, which is unbounded memory in an append-only registry. Rejecting
// non-token method names keeps the label interface bounded without eRPC
// committing to WHICH tokens are real.

// MaxMethodNameLength bounds a JSON-RPC method name. The longest method in the
// wild is well under 50 bytes (`eth_getTransactionByBlockNumberAndIndex` is
// 39); 128 leaves generous headroom for vendor-namespaced methods while still
// bounding the memory a single label value can retain.
const MaxMethodNameLength = 128

// isValidMethodNameChar reports whether c may appear in a JSON-RPC method
// name. The set is ASCII alphanumerics plus `_` (the near-universal namespace
// separator), `.` (OpenRPC's reserved `rpc.` prefix) and `-`.
func isValidMethodNameChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_' || c == '.' || c == '-'
}

// IsValidMethodName reports whether method is a plausible JSON-RPC method
// name: non-empty, at most MaxMethodNameLength bytes, and made only of
// isValidMethodNameChar bytes. It allocates nothing and never scans past the
// first offending byte, so hostile input costs less than valid input.
func IsValidMethodName(method string) bool {
	if len(method) == 0 || len(method) > MaxMethodNameLength {
		return false
	}
	// Indexed by byte, not by rune: a multi-byte UTF-8 sequence must fail on
	// its first byte rather than be skipped over by a `range` over the string.
	for i := range len(method) {
		if !isValidMethodNameChar(method[i]) {
			return false
		}
	}
	return true
}

// truncateForError caps an untrusted string before it is embedded in an error
// message, so a hostile method name cannot inflate the error, the log line and
// the response body it ends up in.
func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// noCopySearch locates a value inside a request body without copying it.
//
// The default `sonic.Get` is GetCopyFromString: it allocates a copy of whatever
// it finds. On a 128 KiB `eth_sendRawTransaction` that is a 128 KiB allocation
// just to learn that `params` is an array — and, for a hostile 128 KiB method
// name, a 128 KiB allocation before anything gets the chance to reject it.
// Node.Type() is a bit-mask over the value's first byte and Node.Raw() returns
// the referenced substring, so neither needs the copy. Nodes produced with
// these options alias the request body and must not outlive it.
//
// ValidateJSON stays on. It is not about catching syntax errors — the key
// scanner reports those either way — it selects sonic's SIMD skip over the
// scalar one, which halves the cost of stepping over a large `params` value
// (128 KiB: 4.9µs vs 10.1µs) for ~25ns on a typical body. Bounded tail beats
// the median.
var noCopySearch = ast.SearchOptions{
	ValidateJSON:   true,
	CopyReturn:     false,
	ConcurrentRead: false,
}

// maxMethodRawLength bounds the raw JSON token eRPC will decode into a method
// string. The shortest escape sequence (`\uXXXX`, 6 bytes) yields at least one
// decoded byte, so a token longer than 6×MaxMethodNameLength (plus the two
// quotes) cannot possibly decode to a valid name — rejecting it early bounds
// the decode without ever over-rejecting a legal escaped name.
const maxMethodRawLength = 6*MaxMethodNameLength + 2

// errInvalidMethodName builds the single rejection reason for a method that is
// not a plausible JSON-RPC method name. The offending value is echoed verbatim
// but truncated: it is attacker-controlled and ends up in an error string, a
// log line and a response body.
func errInvalidMethodName(method string) error {
	return NewErrInvalidRequest(fmt.Errorf(
		"method must be 1-%d characters of [a-zA-Z0-9_.-], got: %q",
		MaxMethodNameLength, truncateForError(method, 64),
	))
}
