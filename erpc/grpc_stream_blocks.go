package erpc

import (
	"encoding/json"
	"fmt"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/indexer"
	"google.golang.org/protobuf/proto"
)

// streamBlocksAuthMethod is the method consumer auth checks a StreamBlocks
// open against: the stream serves blocks by number, so allowlists that
// name eth_getBlockByNumber admit it.
const streamBlocksAuthMethod = "eth_getBlockByNumber"

// streamBlocksEncoding is each pinned header's StreamBlocks message,
// encoded once for every stream.
var streamBlocksEncoding = evm.NewHeaderEncoding("grpc", streamBlocksResponse)

// streamEncodeError is a held block the stream cannot encode: sending past
// it would break the parentHash linkage the client relies on.
type streamEncodeError struct {
	number uint64
	err    error
}

func (e streamEncodeError) Error() string {
	return fmt.Sprintf("cannot encode block %d: %v", e.number, e.err)
}

// StreamBlocks is a stream session on the network-wide chain tracker
// (CT§10.2, §10.3; streams spec §2, §6.1): it sends each applied block's
// header, oldest first, up to the finality bound. After a reorg the next
// header sent is the fork point + 1; reverted blocks are never sent, nor
// is a block that left the pins before its send. A request that sets
// heartbeatSeconds gets heartbeat and status messages too; one that sets
// no field from 5 on gets header messages only, encoded as before the
// oneof. Auth and rate limits run once, at open: auth as
// eth_getBlockByNumber, rate limits as for ws newHeads subscriptions.
func (gs *GrpcServer) StreamBlocks(req *bdsevm.StreamBlocksRequest, stream bdsevm.StreamService_StreamBlocksServer) error {
	ctx := stream.Context()
	start, err := grpcStreamStart(req.FromBlock, req.FromHash)
	if err != nil {
		return err
	}
	own := map[string]any{}
	if err := grpcFinality(own, req.Finality, req.Depth); err != nil {
		return err
	}
	grpcHeartbeat(own, req.HeartbeatSeconds)
	opts, err := grpcStreamOptions(indexer.SubTypeNewHeads, own)
	if err != nil {
		return err
	}
	opts.From = start
	nw, err := gs.openGrpcStream(ctx, streamBlocksAuthMethod, indexer.SubTypeNewHeads, opts)
	if err != nil {
		return err
	}
	sender := newStreamSender(stream, "StreamBlocks")
	defer sender.stop()
	w := &grpcWriter{sender: sender, wait: grpcSendWait.Load(), max: gs.maxMessage, meter: newStreamMeter(nw, "grpc", indexer.SubTypeNewHeads)}
	s := newStreamSession(nw, "grpc", indexer.SubTypeNewHeads, "", opts)
	s.headers = &grpcBlocksEncoder{tracker: nw.chainTracker, w: w}
	if req.HeartbeatSeconds != nil {
		s.control = &grpcControl{grpcWriter: w,
			heartbeat: func(h *bdsevm.Heartbeat) proto.Message {
				return &bdsevm.StreamBlocksResponse{Message: &bdsevm.StreamBlocksResponse_Heartbeat{Heartbeat: h}}
			},
			status: func(st *bdsevm.StreamStatus) proto.Message {
				return &bdsevm.StreamBlocksResponse{Message: &bdsevm.StreamBlocksResponse_Status{Status: st}}
			},
		}
	}
	return gs.runGrpcSession(ctx, nw, s)
}

// grpcBlocksEncoder is the StreamBlocks encoder: one header message per
// block.
type grpcBlocksEncoder struct {
	tracker *evm.ChainTracker
	w       *grpcWriter
}

func (e *grpcBlocksEncoder) Header(blk indexer.Block) (func(tapDelivery) error, bool, error) {
	msg, pinned, err := streamBlocksEncoding.Of(e.tracker, blk)
	if err != nil {
		return nil, pinned, streamEncodeError{blk.Number, err}
	}
	if !pinned {
		// The session holds blk: no message.
		return nil, false, nil
	}
	if err := e.w.fits(blk, msg); err != nil {
		return nil, pinned, err
	}
	return func(d tapDelivery) error {
		if err := e.w.write(streamData, msg); err != nil {
			return err
		}
		d.done()
		return nil
	}, pinned, nil
}

// streamBlocksResponse is the StreamBlocks message of a held header.
func streamBlocksResponse(header json.RawMessage) (*grpcEncoded, error) {
	h, err := evm.StreamBlockHeader(header)
	if err != nil {
		return nil, err
	}
	return encodeGrpc(&bdsevm.StreamBlocksResponse{Message: &bdsevm.StreamBlocksResponse_Header{Header: h}})
}
