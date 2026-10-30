package erpc

import (
	"context"
	"encoding/json"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	w := &grpcWriter{sender: sender, wait: grpcSendWait.Load(), meter: newStreamMeter(nw, "grpc", indexer.SubTypeNewPendingTransactions)}
	control := &grpcControl{grpcWriter: w,
		heartbeat: func(h *bdsevm.Heartbeat) proto.Message {
			return &bdsevm.StreamPendingTransactionsResponse{Message: &bdsevm.StreamPendingTransactionsResponse_Heartbeat{Heartbeat: h}}
		},
	}
	enc := &grpcPendingEncoder{w: w, full: req.FullTransactions, logger: gs.logger}
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
	w      *grpcWriter
	full   bool
	logger *zerolog.Logger
}

// Pending writes txs, each a hash or a whole transaction as the variant
// holds it; one that does not convert is left out, logged.
func (e *grpcPendingEncoder) Pending(txs []json.RawMessage) error {
	var hashes [][]byte
	var full []*bdsevm.Transaction
	for _, raw := range txs {
		if e.full {
			var m map[string]interface{}
			if err := common.SonicCfg.Unmarshal(raw, &m); err != nil {
				e.logger.Warn().Err(err).Msg("cannot read a pending transaction")
				continue
			}
			tx, err := bdsevm.ParseJsonRpcTransaction(m, nil)
			if err != nil {
				e.logger.Warn().Err(err).Msg("cannot convert a pending transaction to proto")
				continue
			}
			full = append(full, tx)
			continue
		}
		var h string
		if err := common.SonicCfg.Unmarshal(raw, &h); err != nil {
			e.logger.Warn().Err(err).Msg("cannot read a pending transaction hash")
			continue
		}
		b, err := bdsevm.HexToBytes(h)
		if err != nil {
			e.logger.Warn().Err(err).Str("hash", h).Msg("cannot convert a pending transaction hash")
			continue
		}
		hashes = append(hashes, b)
	}
	n, size := len(hashes), func(i int) int { return len(hashes[i]) + 2 }
	if e.full {
		n, size = len(full), func(i int) int { return proto.Size(full[i]) + 4 }
	}
	if n == 0 {
		return nil
	}
	return grpcBatches(n, size, func(from, to int) error {
		batch := &bdsevm.PendingTransactions{}
		if e.full {
			batch.Transactions = full[from:to]
		} else {
			batch.Hashes = hashes[from:to]
		}
		return e.w.writeProto(streamData, &bdsevm.StreamPendingTransactionsResponse{
			Message: &bdsevm.StreamPendingTransactionsResponse_Transactions{Transactions: batch},
		})
	})
}
