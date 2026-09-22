package indexer

import (
	"encoding/json"

	"github.com/erpc/erpc/common"
)

// logRemoved extracts the "removed" flag from a log notification payload,
// defaulting to false on parse failure. Isolated into its own helper so
// the hot-path Ingest doesn't carry an inline json unmarshal.
func logRemoved(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var probe struct {
		Removed bool `json:"removed"`
	}
	if err := common.SonicCfg.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return probe.Removed
}
