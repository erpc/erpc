package clients

import (
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
)

var batchBodySink []byte

func benchmarkBatchBodies(count int) ([][]byte, []common.JsonRpcRequest) {
	rawBodies := make([][]byte, 0, count)
	legacyReqs := make([]common.JsonRpcRequest, 0, count)

	for i := 0; i < count; i++ {
		raw := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","0x15536ea"]}`, i+1))
		rawBodies = append(rawBodies, raw)

		legacyReqs = append(legacyReqs, common.JsonRpcRequest{
			JSONRPC: "2.0",
			ID:      i + 1,
			Method:  "eth_getBalance",
			Params: []interface{}{
				"0x0000000000000000000000000000000000000001",
				"0x15536ea",
			},
		})
	}

	return rawBodies, legacyReqs
}

func benchmarkBatchBodyBuilders(b *testing.B, count int) {
	b.Run(fmt.Sprintf("fast_join_raw_%d", count), func(b *testing.B) {
		rawBodies, _ := benchmarkBatchBodies(count)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			batchBodySink = buildJsonRpcBatchRequestBody(rawBodies)
		}
	})

	b.Run(fmt.Sprintf("legacy_marshal_array_%d", count), func(b *testing.B) {
		_, legacyReqs := benchmarkBatchBodies(count)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			body, err := common.SonicCfg.Marshal(legacyReqs)
			if err != nil {
				b.Fatal(err)
			}
			batchBodySink = body
		}
	})
}

func BenchmarkBuildJsonRpcBatchRequestBody(b *testing.B) {
	benchmarkBatchBodyBuilders(b, 10)
	benchmarkBatchBodyBuilders(b, 100)
}
