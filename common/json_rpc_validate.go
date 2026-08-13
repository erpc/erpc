package common

import (
	"fmt"
)

// MaxMethodNameLength bounds a JSON-RPC method name. The longest method in the
// wild is well under 50 bytes (`eth_getTransactionByBlockNumberAndIndex` is 39).
const MaxMethodNameLength = 128

// IsValidMethodName reports whether method is shaped like a JSON-RPC method
// name: 1 to MaxMethodNameLength bytes of [a-zA-Z0-9_.-].
//
// This is a shape check, not an allowlist. Methods are an open set and any
// token of that shape passes. It rejects input that is not a method name at
// all, such as SQL or script payloads glued onto `eth_call` by scanners, which
// would otherwise be forwarded upstream and used as the `category` label.
// It does not bound label cardinality: distinct well-shaped tokens still mint
// distinct series.
func IsValidMethodName(method string) bool {
	if len(method) == 0 || len(method) > MaxMethodNameLength {
		return false
	}
	// Indexed by byte, not by rune, so a multi-byte UTF-8 sequence fails on its
	// first byte.
	for i := range len(method) {
		c := method[i]
		if !((c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// errInvalidMethodName echoes the offending method truncated to 64 bytes: it is
// attacker-controlled and ends up in the response body and the logs.
func errInvalidMethodName(method string) error {
	const maxEcho = 64
	if len(method) > maxEcho {
		method = method[:maxEcho] + "..."
	}
	return NewErrInvalidRequest(fmt.Errorf(
		"method must be 1-%d characters of [a-zA-Z0-9_.-], got: %q",
		MaxMethodNameLength, method,
	))
}
