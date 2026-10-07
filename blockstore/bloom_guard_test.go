package blockstore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An upstream header with a malformed logsBloom is never admitted, and logs
// validation never feeds an unchecked bloom to types.BytesToBloom, which
// panics on more than 256 bytes (a process crash inside the payload
// singleflight).
func TestLogsBloom_MalformedIsRejectedNotPanicking(t *testing.T) {
	ch := newFakeChain(5)
	good := headerOf(t, ch, 5)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(good, &m))
	withBloom := func(b string) json.RawMessage {
		m["logsBloom"] = json.RawMessage(`"` + b + `"`)
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		return raw
	}
	for name, bloom := range map[string]string{
		"257 bytes":       "0x" + strings.Repeat("ab", 257),
		"odd 513 hex":     "0x" + strings.Repeat("a", 513),
		"short":           "0x" + strings.Repeat("00", 255),
		"not hex":         "0x" + strings.Repeat("zz", 256),
		"no prefix":       strings.Repeat("00", 257),
		"huge (64KB hex)": "0x" + strings.Repeat("ff", 1<<15),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseHeader(withBloom(bloom), 5)
			require.ErrorContains(t, err, "logsBloom", "rejected at admission")

			// Defense in depth: even a block that skipped admission does not panic.
			b := &rawBlock{Number: "0x5", Hash: hashOf(5, "a"), LogsBloom: bloom}
			require.NotPanics(t, func() {
				require.Error(t, validateCompleteLogs(b, 5, map[string]struct{}{}, json.RawMessage(`[]`)))
			})
		})
	}
	_, err := parseHeader(withBloom("0x"+strings.Repeat("00", 256)), 5)
	require.NoError(t, err, "a well-formed (empty) bloom is admitted")
}
