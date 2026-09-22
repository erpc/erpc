package common

import "testing"

var benchmarkCacheHashSink string

func BenchmarkCacheHash_Simple(b *testing.B) {
	req := NewJsonRpcRequest(
		"eth_getBalance",
		[]interface{}{
			"0x0000000000000000000000000000000000000001",
			"latest",
		},
	)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req.InvalidateCacheHash()

		hash, err := req.CacheHash()
		if err != nil {
			b.Fatal(err)
		}

		benchmarkCacheHashSink = hash
	}
}

func BenchmarkCacheHash_EthGetLogs(b *testing.B) {
	req := NewJsonRpcRequest(
		"eth_getLogs",
		[]interface{}{
			map[string]interface{}{
				"fromBlock": "0x64",
				"toBlock":   "0x64",
				"address":   "0x0000000000000000000000000000000000000001",
				"topics": []interface{}{
					[]interface{}{
						"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					},
				},
			},
		},
	)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req.InvalidateCacheHash()

		hash, err := req.CacheHash()
		if err != nil {
			b.Fatal(err)
		}

		benchmarkCacheHashSink = hash
	}
}
