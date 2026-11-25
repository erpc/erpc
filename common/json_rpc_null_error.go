package common

import "bytes"

// isJsonNull reports whether raw is the JSON literal null.
//
// JSON-RPC 1.0 requires both members on every response, so a success carries
// "error": null. Some JSON-RPC 2.0 servers send the null member too. A null
// error member therefore means "no error".
//
// The two response parse sites call this before ParseError, because they know
// they hold the error member. ParseError itself stays unchanged: the HTTP
// client also calls it with a whole response body that failed to parse, and
// a bare null body there is a malformed response, not a success.
func isJsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
