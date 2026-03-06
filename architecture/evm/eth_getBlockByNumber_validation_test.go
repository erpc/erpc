package evm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockValidationGroundTruthLogFixture struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
}

const blockValidationAddressOnlyBloom = "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000080000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

func loadBlockValidationBlockFixture(tb testing.TB) []byte {
	tb.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "block_validation_block.json"))
	require.NoError(tb, err)
	return data
}

func loadBlockValidationGroundTruthLogsFixture(tb testing.TB) []*common.GroundTruthLog {
	tb.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "block_validation_logs.json"))
	require.NoError(tb, err)

	var raw []*blockValidationGroundTruthLogFixture
	require.NoError(tb, common.SonicCfg.Unmarshal(data, &raw))

	logs := make([]*common.GroundTruthLog, len(raw))
	for i, log := range raw {
		if log == nil {
			continue
		}

		address, err := common.HexToBytes(log.Address)
		require.NoError(tb, err)

		topics := make([][]byte, len(log.Topics))
		for j, topic := range log.Topics {
			topics[j], err = common.HexToBytes(topic)
			require.NoError(tb, err)
		}

		logs[i] = &common.GroundTruthLog{
			Address: address,
			Topics:  topics,
		}
	}

	return logs
}

func newBlockValidationResponseFromFixture(tb testing.TB) *common.NormalizedResponse {
	tb.Helper()

	jrpcResp, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), loadBlockValidationBlockFixture(tb), nil)
	require.NoError(tb, err)

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrpcResp)
}

func newBlockValidationRequest(dirs *common.RequestDirectives) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`))
	req.SetDirectives(dirs)
	return req
}

func TestValidateBlock_LogsBloomEmptinessWithGroundTruthLogs(t *testing.T) {
	ctx := context.Background()

	t.Run("RejectsNonZeroBloomWithExplicitEmptyLogs", func(t *testing.T) {
		resp := newBlockValidationResponseFromFixture(t)
		dirs := &common.RequestDirectives{
			ValidateLogsBloomEmptiness: true,
			GroundTruthLogs:            []*common.GroundTruthLog{},
		}
		req := newBlockValidationRequest(dirs)
		resp.WithRequest(req)

		err := validateBlock(ctx, nil, dirs, req, resp)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-zero")
		assert.Contains(t, err.Error(), "ground-truth log records")
	})

	t.Run("AcceptsMatchingNonZeroBloom", func(t *testing.T) {
		resp := newBlockValidationResponseFromFixture(t)
		dirs := &common.RequestDirectives{
			ValidateLogsBloomEmptiness: true,
			GroundTruthLogs:            loadBlockValidationGroundTruthLogsFixture(t),
		}
		req := newBlockValidationRequest(dirs)
		resp.WithRequest(req)

		err := validateBlock(ctx, nil, dirs, req, resp)
		require.NoError(t, err)
	})
}

func TestValidateBlock_LogsBloomMatchWithGroundTruthLogs(t *testing.T) {
	ctx := context.Background()

	t.Run("AcceptsMatchingBloom", func(t *testing.T) {
		resp := newBlockValidationResponseFromFixture(t)
		dirs := &common.RequestDirectives{
			ValidateLogsBloomMatch: true,
			GroundTruthLogs:        loadBlockValidationGroundTruthLogsFixture(t),
		}
		req := newBlockValidationRequest(dirs)
		resp.WithRequest(req)

		err := validateBlock(ctx, nil, dirs, req, resp)
		require.NoError(t, err)
	})

	t.Run("RejectsMismatchingBloom", func(t *testing.T) {
		resp := newBlockValidationResponseFromFixture(t)
		logs := loadBlockValidationGroundTruthLogsFixture(t)
		logs[0].Topics[0][0] ^= 0x01

		dirs := &common.RequestDirectives{
			ValidateLogsBloomMatch: true,
			GroundTruthLogs:        logs,
		}
		req := newBlockValidationRequest(dirs)
		resp.WithRequest(req)

		err := validateBlock(ctx, nil, dirs, req, resp)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match ground-truth logs")
	})
}

func TestValidateBlock_LogsBloomValidationSkipsWithoutGroundTruthLogs(t *testing.T) {
	ctx := context.Background()

	resp := newBlockValidationResponseFromFixture(t)
	dirs := &common.RequestDirectives{
		ValidateLogsBloomEmptiness: true,
		ValidateLogsBloomMatch:     true,
	}
	req := newBlockValidationRequest(dirs)
	resp.WithRequest(req)

	err := validateBlock(ctx, nil, dirs, req, resp)
	require.NoError(t, err)
}

func cloneGroundTruthLogs(logs []*common.GroundTruthLog) []*common.GroundTruthLog {
	cloned := make([]*common.GroundTruthLog, len(logs))
	for i, log := range logs {
		if log == nil {
			continue
		}

		clonedLog := &common.GroundTruthLog{
			Address: append([]byte(nil), log.Address...),
		}
		if len(log.Topics) > 0 {
			clonedLog.Topics = make([][]byte, len(log.Topics))
			for j, topic := range log.Topics {
				clonedLog.Topics[j] = append([]byte(nil), topic...)
			}
		}

		cloned[i] = clonedLog
	}

	return cloned
}

func repeatGroundTruthLogs(logs []*common.GroundTruthLog, repeats int) []*common.GroundTruthLog {
	repeated := make([]*common.GroundTruthLog, 0, len(logs)*repeats)
	for i := 0; i < repeats; i++ {
		repeated = append(repeated, cloneGroundTruthLogs(logs)...)
	}
	return repeated
}

func makeAddressOnlyGroundTruthLogs(logs []*common.GroundTruthLog, repeats int) []*common.GroundTruthLog {
	repeated := make([]*common.GroundTruthLog, 0, len(logs)*repeats)
	for i := 0; i < repeats; i++ {
		for _, log := range logs {
			if log == nil {
				continue
			}
			repeated = append(repeated, &common.GroundTruthLog{
				Address: append([]byte(nil), log.Address...),
			})
		}
	}
	return repeated
}

func BenchmarkValidateBlockLogsBloomMatch(b *testing.B) {
	baseLogs := loadBlockValidationGroundTruthLogsFixture(b)
	baseResp := newBlockValidationResponseFromFixture(b)
	ctx := context.Background()

	var baseBlock blockValidationBlockLite
	jrr, err := baseResp.JsonRpcResponse(ctx)
	require.NoError(b, err)
	require.NoError(b, common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &baseBlock))

	benchmarks := []struct {
		name  string
		logs  []*common.GroundTruthLog
		block blockValidationBlockLite
	}{
		{
			name:  "1_log_3_topics",
			logs:  baseLogs,
			block: baseBlock,
		},
		{
			name:  "100_logs_3_topics",
			logs:  repeatGroundTruthLogs(baseLogs, 100),
			block: baseBlock,
		},
		{
			name:  "80000_logs_0_topics",
			logs:  makeAddressOnlyGroundTruthLogs(baseLogs, 80000),
			block: blockValidationBlockLite{LogsBloom: blockValidationAddressOnlyBloom},
		},
		{
			name:  "80000_logs_3_topics",
			logs:  repeatGroundTruthLogs(baseLogs, 80000),
			block: baseBlock,
		},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			dirs := &common.RequestDirectives{
				ValidateLogsBloomMatch: true,
				GroundTruthLogs:        bm.logs,
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := validateBlockLogsBloom(nil, dirs, &bm.block)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
