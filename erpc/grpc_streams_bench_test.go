package erpc

import (
	"context"
	"fmt"
	"net"
	"testing"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/indexer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
)

// benchLogsServer sends one message n times on each StreamLogs call.
type benchLogsServer struct {
	bdsevm.UnimplementedStreamServiceServer
	msg any
	n   int
}

func (b *benchLogsServer) StreamLogs(_ *bdsevm.StreamLogsRequest, stream bdsevm.StreamService_StreamLogsServer) error {
	for range b.n {
		if err := stream.SendMsg(b.msg); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkGrpcStreamLogsSend is the per-subscriber cost of sending one
// 1,000-log block message over loopback gRPC with the server's tuning and
// a toolkit client's windows (streams spec §6.2, §6.3): encoded once per
// block from shared log buffers, as StreamLogs sends it, against a proto
// message the codec marshals on every send. The client reads raw bytes,
// so its cost stays out of the comparison. held-B/filter is what one
// filter's encoding of the block holds; copy-B/filter what it would hold
// as one contiguous message.
func BenchmarkGrpcStreamLogsSend(b *testing.B) {
	logs := make([]mem.Buffer, 1000)
	for i := range logs {
		raw := fmt.Appendf(nil, `{"address":"0x00000000000000000000000000000000000000a1","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef","0x000000000000000000000000000000000000000000000000000000000000beef","0x000000000000000000000000000000000000000000000000000000000000cafe"],"data":"0x00000000000000000000000000000000000000000000000000000000000f4240","blockNumber":"0x1406f40","blockHash":"0x77aa000000000000000000000000000000000000000000000000000000000001","blockTimestamp":"0x65f0a1b2","transactionHash":"0x%064x","transactionIndex":"0x%x","logIndex":"0x%x","removed":false}`, i, i/2, i)
		l, err := grpcLog(raw)
		if err != nil {
			b.Fatal(err)
		}
		logs[i] = l
	}
	msgs, held, err := grpcBlockLogs(indexer.Block{Number: 0x1406f40, Hash: [32]byte{0x77, 0xaa}, ParentHash: [32]byte{0x5c, 0x1e}}, logs)
	if err != nil || len(msgs) != 1 {
		b.Fatalf("one message, got %d: %v", len(msgs), err)
	}
	pre := msgs[0]
	flat := pre.bufs.Materialize()
	var decoded bdsevm.StreamLogsResponse
	if err := proto.Unmarshal(flat, &decoded); err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		msg  any
	}{{"pre-encoded", pre}, {"marshal-per-send", &decoded}} {
		b.Run(tc.name, func(b *testing.B) {
			srv := &benchLogsServer{msg: tc.msg, n: b.N}
			gs := grpc.NewServer(grpc.ForceServerCodecV2(newGrpcCodec()), grpc.SharedWriteBuffer(true))
			bdsevm.RegisterStreamServiceServer(gs, srv)
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			go func() { _ = gs.Serve(lis) }()
			defer gs.Stop()
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithInitialWindowSize(4<<20), grpc.WithInitialConnWindowSize(16<<20))
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			b.SetBytes(int64(pre.size))
			b.ReportAllocs()
			b.ResetTimer()
			st, err := conn.NewStream(context.Background(), &bdsevm.StreamService_ServiceDesc.Streams[1], "/bds.evm.StreamService/StreamLogs",
				grpc.ForceCodecV2(rawCodec{encoding.GetCodecV2("proto")}))
			if err != nil {
				b.Fatal(err)
			}
			if err := st.SendMsg(&bdsevm.StreamLogsRequest{}); err != nil {
				b.Fatal(err)
			}
			_ = st.CloseSend()
			for range b.N {
				var got []byte
				if err := st.RecvMsg(&got); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(held), "held-B/filter")
			b.ReportMetric(float64(len(flat)), "copy-B/filter")
		})
	}
}
