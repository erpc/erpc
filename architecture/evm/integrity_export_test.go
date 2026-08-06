package evm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The archive is the durable forensic record of every catch: one JSONL line
// with upstream/check/verbatim reason and the offending body.
func TestExportIntegrityCatch_WritesJSONL(t *testing.T) {
	dir := t.TempDir()
	l := zerolog.Nop()
	exp := consensus.NewMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{
		Type: common.MisbehaviorsDestinationTypeFile,
		Path: dir,
	}, &l)
	require.NotNil(t, exp)

	rec := integrityCatchRecord{
		Timestamp: "2026-07-03T00:00:00Z",
		Project:   "main", Network: "evm:1", Upstream: "u1", Vendor: "v1",
		Method: "eth_getBlockByNumber", Check: "transactionsRootConsistency",
		Class: "deterministic", Verdict: "reject", Finality: "finalized",
		Reason:   "non-empty transactionsRoot but empty transactions",
		Response: `{"hash":"0xbb","transactions":[]}`,
	}
	line, err := common.SonicCfg.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, exp.AppendWithMetadata(line, rec.Method, rec.Network))

	files, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	content, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var got integrityCatchRecord
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(content))), &got))
	assert.Equal(t, "transactionsRootConsistency", got.Check)
	assert.Equal(t, "reject", got.Verdict)
	assert.Contains(t, got.Response, "0xbb")
}

// An empty-response record marshals cleanly (response omitted, not "").
func TestIntegrityCatchRecord_MarshalOmitsEmptyResponse(t *testing.T) {
	line, err := common.SonicCfg.Marshal(integrityCatchRecord{Check: "x", Verdict: "soft_flag"})
	require.NoError(t, err)
	assert.NotContains(t, string(line), `"response"`)
}
