package integrity

import (
	"encoding/json"
	"strings"
	"sync"
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

// headerFormats are the registered formats; a chain with its own format
// adds one here. VerifyBlockHash tries the format a chain last verified
// with first, then these in order.
var headerFormats = []HeaderFormat{
	{Name: "ethereum", Hash: ethereumBlockHash},
	{Name: "arbitrum-nitro", Hash: arbitrumNitroBlockHash},
	{Name: "bsc", Hash: bscBlockHash},
	{Name: "opbnb", Hash: opbnbBlockHash},
	{Name: "skale", Hash: skaleBlockHash},
	{Name: "sonic", Hash: sonicBlockHash},
	{Name: "morph", Hash: morphBlockHash},
	{Name: "citrea", Hash: citreaBlockHash},
	{Name: "megaeth", Hash: megaethBlockHash},
	{Name: "base-timestamp-ms", Hash: baseTimestampMsBlockHash},
	{Name: "coreth", Hash: corethBlockHash},
	{Name: "subnet-evm", Hash: subnetEVMBlockHash},
	{Name: "berachain", Hash: berachainBlockHash},
	{Name: "xdpos", Hash: xdposBlockHash},
	{Name: "fuse", Hash: fuseBlockHash},
	{Name: "frontier", Hash: frontierBlockHash},
	{Name: "lightlink", Hash: lightlinkBlockHash},
	{Name: "tempo", Hash: tempoBlockHash},
	{Name: "rootstock", Hash: rootstockBlockHash},
	{Name: "etherlink", Hash: etherlinkBlockHash},
	{Name: "aurora", Hash: auroraBlockHash},
	{Name: "zksync", Hash: zksyncBlockHash},
}

// lastFormat is, per chain id, the index in headerFormats of the format
// its last verified header took. Chain id 0 (unknown) learns none.
var lastFormat sync.Map // int64 -> int

// VerifyBlockHash parses raw once and tries the format chainID last
// verified with, then each registered format in order: format is the name
// of the first that reproduces raw's claimed hash, "" when none does.
// encodable is whether the chain's format derives a hash from raw: the
// format it last verified with, else any registered format. A header that
// format cannot encode lacks keys it needs or carries keys it does not
// know, as from a source that re-serializes blocks: its hash is unproven,
// even when the standard format encodes the header to another hash.
func VerifyBlockHash(chainID int64, raw []byte) (format string, encodable bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", false
	}
	var claimed string
	if err := json.Unmarshal(fields["hash"], &claimed); err != nil || claimed == "" {
		return "", false
	}
	last := -1
	if v, ok := lastFormat.Load(chainID); ok {
		last = v.(int)
	}
	lastEncodes, anyEncodes := false, false
	try := func(i int) bool {
		got, ok := headerFormats[i].Hash(chainID, fields)
		if i == last {
			lastEncodes = ok
		} else {
			anyEncodes = anyEncodes || ok
		}
		return ok && strings.EqualFold(got, claimed)
	}
	if last >= 0 && try(last) {
		return headerFormats[last].Name, true
	}
	for i := range headerFormats {
		if i != last && try(i) {
			if chainID != 0 {
				lastFormat.Store(chainID, i)
			}
			return headerFormats[i].Name, true
		}
	}
	if last >= 0 {
		return "", lastEncodes
	}
	return "", anyEncodes
}
