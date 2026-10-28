package clients

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"google.golang.org/protobuf/proto"
)

// loadBaseReceipts returns Base block 53207078's receipts (415, 1.39 MB as
// the node sent them) as a BDS server's GetBlockReceipts answer.
func loadBaseReceipts(b *testing.B) *evm.GetBlockReceiptsResponse {
	b.Helper()
	f, err := os.Open("testdata/base-53207078-receipts.json.gz")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		b.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		b.Fatal(err)
	}
	var nodeReceipts []*evm.JsonRpcReceipt
	if err := json.Unmarshal(raw, &nodeReceipts); err != nil {
		b.Fatal(err)
	}
	resp := &evm.GetBlockReceiptsResponse{}
	for _, nr := range nodeReceipts {
		r, err := nr.ToProto()
		if err != nil {
			b.Fatal(err)
		}
		resp.Receipts = append(resp.Receipts, r)
	}
	wire, err := proto.Marshal(resp)
	if err != nil {
		b.Fatal(err)
	}
	out := &evm.GetBlockReceiptsResponse{}
	if err := proto.Unmarshal(wire, out); err != nil {
		b.Fatal(err)
	}
	return out
}

// The eth_getBlockReceipts result rendering, as handleGetBlockReceipts does it.
func BenchmarkGrpcBdsReceiptsJson(b *testing.B) {
	resp := loadBaseReceipts(b)
	render := func() []byte {
		return renderResult(func(buf []byte) []byte { return evm.AppendReceiptsJsonRpc(buf, resp.Receipts) })
	}
	b.SetBytes(int64(len(render())))
	b.ReportAllocs()
	for b.Loop() {
		render()
	}
}
