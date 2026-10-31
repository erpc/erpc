package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// StreamPendingTransactions streams the network's mempool view (CT§12;
// streams spec §6.1, §6.2): one PendingTransactions per mempool Read, at
// most grpcBatchMax per message, hashes or, with fullTransactions, whole
// transactions. It shares the ws variant of the same params, so ws and
// gRPC subscribers share one upstream subscription. Heartbeats are always
// on; no cursor, no reorgs. Auth and rate limits as eth_subscribe, once at
// open.
func (gs *GrpcServer) StreamPendingTransactions(req *bdsevm.StreamPendingTransactionsRequest, stream bdsevm.StreamService_StreamPendingTransactionsServer) error {
	ctx := stream.Context()
	own := map[string]any{}
	grpcHeartbeat(own, req.HeartbeatSeconds)
	opts, err := grpcStreamOptions(indexer.SubTypeNewPendingTransactions, own)
	if err != nil {
		return err
	}
	nw, err := gs.openGrpcStream(ctx, MethodEthSubscribe, indexer.SubTypeNewPendingTransactions, opts)
	if err != nil {
		return err
	}
	params := []interface{}{indexer.SubTypeNewPendingTransactions}
	if req.FullTransactions {
		params = append(params, true)
	}
	variant, release, err := openPendingVariant(ctx, nw, params)
	if err != nil {
		return grpcStatusOf(err)
	}
	defer release()
	from := nw.mempool.Tail(variant)

	tapCtx, stop := context.WithCancel(nw.appCtx)
	defer stop()
	defer context.AfterFunc(ctx, stop)()
	sender := newStreamSender(stream, "StreamPendingTransactions")
	defer sender.stop()
	w := &grpcWriter{sender: sender, wait: grpcSendWait.Load(), max: gs.maxMessage, meter: newStreamMeter(nw, "grpc", indexer.SubTypeNewPendingTransactions)}
	control := &grpcControl{grpcWriter: w,
		heartbeat: func(h *bdsevm.Heartbeat) proto.Message {
			return &bdsevm.StreamPendingTransactionsResponse{Message: &bdsevm.StreamPendingTransactionsResponse_Heartbeat{Heartbeat: h}}
		},
	}
	enc := &grpcPendingEncoder{w: w, full: req.FullTransactions, network: nw.Label(), logger: gs.logger}
	done := make(chan *streamEnd, 1)
	if !nw.bg.Go(func() {
		done <- runPendingTap(tapCtx, nw, "grpc", variant, from, nil, gs.appCtx.Done(), opts, control.Heartbeat, enc.Pending)
	}) {
		return status.Error(codes.Unavailable, "network is stopping")
	}
	return gs.grpcEnd(ctx, tapCtx, nw, indexer.SubTypeNewPendingTransactions, <-done)
}

// grpcPendingEncoder writes a mempool Read's batch as PendingTransactions
// messages.
type grpcPendingEncoder struct {
	w       *grpcWriter
	full    bool
	network string
	logger  *zerolog.Logger
}

// The wire fields a pending message holds.
var (
	fieldPendingResponseTransactions = fieldNumber(&bdsevm.StreamPendingTransactionsResponse{}, "transactions")
	fieldPendingHashes               = fieldNumber(&bdsevm.PendingTransactions{}, "hashes")
	fieldPendingTransactions         = fieldNumber(&bdsevm.PendingTransactions{}, "transactions")
)

// Pending writes txs, each a hash or a whole transaction as the variant
// holds it; one that does not convert, or whose message alone would pass
// the message bound, is left out, logged (§6.2).
func (e *grpcPendingEncoder) Pending(txs []*indexer.PendingTx) error {
	items := make([]mem.Buffer, 0, len(txs))
	for _, tx := range txs {
		it, err := e.item(tx)
		if err != nil {
			e.logger.Warn().Err(err).Bool("full", e.full).Msg("cannot convert a pending transaction to proto")
			continue
		}
		if n := it.Len(); protowire.SizeTag(fieldPendingResponseTransactions)+protowire.SizeVarint(uint64(n))+n > e.w.max {
			e.logger.Warn().Int("bytes", n).Int("maxBytes", e.w.max).Msg("pending transaction left out: its message passes the message bound")
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil
	}
	return grpcBatches(len(items), func(i int) int { return items[i].Len() }, func(from, to int) error {
		return e.w.write(streamData, grpcMessage(fieldPendingResponseTransactions, nil, items[from:to], nil))
	})
}

// grpcPendingKey keys a pending transaction's gRPC encoding.
type grpcPendingKey struct{ full bool }

type grpcPendingEncoded struct {
	once sync.Once
	item mem.Buffer
	err  error
}

// item is tx as a PendingTransactions element, converted once for every
// stream that sends it (§6.2).
func (e *grpcPendingEncoder) item(tx *indexer.PendingTx) (mem.Buffer, error) {
	key := grpcPendingKey{e.full}
	x, ok := tx.Encodings.Load(key)
	if !ok {
		x, _ = tx.Encodings.LoadOrStore(key, &grpcPendingEncoded{})
	}
	enc := x.(*grpcPendingEncoded)
	enc.once.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"pending-encoding", "network:"+e.network+" transport:grpc", common.ErrorFingerprint(rec),
				).Inc()
				enc.err = fmt.Errorf("conversion panicked: %v", rec)
			}
		}()
		if e.full {
			var p *bdsevm.Transaction
			if p, enc.err = grpcPendingTx(tx.Raw); enc.err == nil {
				enc.item, enc.err = grpcItem(fieldPendingTransactions, p)
			}
			return
		}
		var h string
		if enc.err = common.SonicCfg.Unmarshal(tx.Raw, &h); enc.err != nil {
			return
		}
		b, err := bdsevm.HexToBytes(h)
		if err != nil {
			enc.err = fmt.Errorf("hash %q: %w", h, err)
			return
		}
		enc.item = mem.SliceBuffer(protowire.AppendBytes(protowire.AppendTag(make([]byte, 0, 2+len(b)), fieldPendingHashes, protowire.BytesType), b))
	})
	return enc.item, enc.err
}

// grpcPendingTx is one pending transaction as proto; a var so tests count
// conversions.
var grpcPendingTx = func(raw json.RawMessage) (*bdsevm.Transaction, error) {
	var m map[string]interface{}
	if err := common.SonicCfg.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return bdsevm.ParseJsonRpcTransaction(m, nil)
}
