package clients

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/util"
)

func buildBatchResponseBenchBody(entries int) []byte {
	var sb strings.Builder
	sb.Grow(entries * 72)
	sb.WriteByte('[')
	for i := 0; i < entries; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&sb, `{"jsonrpc":"2.0","id":%d,"result":"0x%x"}`, i+1, i+1)
	}
	sb.WriteByte(']')
	return []byte(sb.String())
}

func parseBatchResponseBench(body string) (int, error) {
	searcher := ast.NewSearcher(body)
	searcher.CopyReturn = false
	searcher.ConcurrentRead = false
	searcher.ValidateJSON = false

	rootNode, err := searcher.GetByPath()
	if err != nil {
		return 0, err
	}

	arrNodes, err := rootNode.ArrayUseNode()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, elemNode := range arrNodes {
		idNode := elemNode.GetByPath("id")
		if _, rawErr := idNode.Raw(); rawErr == nil {
			count++
		}
	}

	return count, nil
}

func BenchmarkBatchResponseParsingWithBodyCopy(b *testing.B) {
	body := buildBatchResponseBenchBody(256)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		count, err := parseBatchResponseBench(string(body))
		if err != nil {
			b.Fatalf("unexpected parse error: %v", err)
		}
		if count != 256 {
			b.Fatalf("unexpected node count: %d", count)
		}
	}
}

func BenchmarkBatchResponseParsingNoBodyCopy(b *testing.B) {
	body := buildBatchResponseBenchBody(256)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		count, err := parseBatchResponseBench(util.B2Str(body))
		if err != nil {
			b.Fatalf("unexpected parse error: %v", err)
		}
		if count != 256 {
			b.Fatalf("unexpected node count: %d", count)
		}
	}
}
