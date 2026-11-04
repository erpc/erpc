package integrity

import (
	"encoding/json"
	"strings"
)

// HeaderFormat is one way chains derive their block hash from the header:
// Hash takes the block JSON object's top-level keys and returns the
// 0x-lowercase hash the format derives; ok is false when the header
// carries a key the format does not encode, lacks one it needs, or holds
// a value it cannot parse. It never panics and ignores the RPC-only keys
// (rpcOnlyBlockFields).
type HeaderFormat struct {
	Name string
	Hash func(chainID int64, fields map[string]json.RawMessage) (hash string, ok bool)
}

// headerFormats are the registered formats, tried in order; a chain with
// its own format adds one here.
var headerFormats = []HeaderFormat{
	{Name: "ethereum", Hash: ethereumBlockHash},
}

// rpcOnlyBlockFields are keys an eth_getBlock* answer carries that no
// header hash covers.
var rpcOnlyBlockFields = map[string]struct{}{
	"hash": {}, "size": {}, "totalDifficulty": {}, "transactions": {},
	"uncles": {}, "withdrawals": {},
}

// VerifyBlockHash parses raw once and tries each registered format: format
// is the name of the first that reproduces raw's claimed hash, "" when
// none does; encodable is whether any format could derive a hash at all.
func VerifyBlockHash(chainID int64, raw []byte) (format string, encodable bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", false
	}
	var claimed string
	if err := json.Unmarshal(fields["hash"], &claimed); err != nil || claimed == "" {
		return "", false
	}
	for _, f := range headerFormats {
		got, ok := f.Hash(chainID, fields)
		if !ok {
			continue
		}
		encodable = true
		if strings.EqualFold(got, claimed) {
			return f.Name, true
		}
	}
	return "", encodable
}
