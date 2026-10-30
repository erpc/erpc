package erpc

import (
	"encoding/json"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// streamLogsAuthMethod is the method consumer auth checks a StreamLogs open
// against (§6.5).
const streamLogsAuthMethod = "eth_getLogs"

// grpcLogsEncoding is each held block's StreamLogs messages per filter:
// each log converted to proto once per block, each (block, filter)'s
// messages referencing those conversions, shared by every StreamLogs
// stream (§6.2).
var grpcLogsEncoding = evm.NewLogsEncoding("grpc", grpcLog, grpcBlockLogs)

// The wire fields grpcBlockLogs writes.
var (
	fieldLogsResponseBlock = fieldNumber(&bdsevm.StreamLogsResponse{}, "block")
	fieldBlockLogsBlock    = fieldNumber(&bdsevm.BlockLogs{}, "block")
	fieldBlockLogsLogs     = fieldNumber(&bdsevm.BlockLogs{}, "logs")
	fieldBlockLogsComplete = fieldNumber(&bdsevm.BlockLogs{}, "complete")
)

// blockLogsComplete is BlockLogs' complete field, set: the tail of a
// block's last message.
var blockLogsComplete mem.Buffer = mem.SliceBuffer(protowire.AppendVarint(
	protowire.AppendTag(nil, fieldBlockLogsComplete, protowire.VarintType), 1))

func fieldNumber(m proto.Message, name string) protowire.Number {
	return m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name)).Number()
}

// StreamLogs is a logs stream session on the network-wide chain tracker
// (streams spec §6.1, §6.2): one BlockLogs per applied block, oldest
// first, empty blocks included; a block with more matched logs than one
// message takes spans messages, the last complete. A reorg of blocks the
// client applied sends Undo with the fork point, then the new branch
// (REORG_UNDO), or ends ABORTED (REORG_NONE with FINALITY_FINALIZED).
// Heartbeats and status are always on. Auth as eth_getLogs, rate limits as
// for ws logs subscriptions, once at open.
func (gs *GrpcServer) StreamLogs(req *bdsevm.StreamLogsRequest, stream bdsevm.StreamService_StreamLogsServer) error {
	ctx := stream.Context()
	start, err := grpcStreamStart(req.FromBlock, req.FromHash)
	if err != nil {
		return err
	}
	filter, err := grpcLogsFilter(req.Filter)
	if err != nil {
		return err
	}
	own := map[string]any{}
	if err := grpcFinality(own, req.Finality, req.Depth); err != nil {
		return err
	}
	switch req.Reorg {
	case bdsevm.Reorg_REORG_UNDO:
		own["reorg"] = "undo"
	case bdsevm.Reorg_REORG_NONE:
		own["reorg"] = "none"
	default:
		return status.Errorf(codes.InvalidArgument, "invalid reorg: unknown value %d", req.Reorg)
	}
	grpcHeartbeat(own, req.HeartbeatSeconds)
	opts, err := grpcStreamOptions(indexer.SubTypeLogs, own)
	if err != nil {
		return err
	}
	opts.From, opts.BlockEnd = start, true
	nw, err := gs.openGrpcStream(ctx, streamLogsAuthMethod, indexer.SubTypeLogs, opts)
	if err != nil {
		return err
	}
	sender := newStreamSender(stream, "StreamLogs")
	defer sender.stop()
	w := &grpcWriter{sender: sender, wait: grpcSendWait.Load(), max: gs.maxMessage, meter: newStreamMeter(nw, "grpc", indexer.SubTypeLogs)}
	s := newStreamSession(nw, "grpc", indexer.SubTypeLogs, "", opts)
	s.filter = filter
	s.logs = &grpcLogsEncoder{tracker: nw.chainTracker, w: w}
	s.control = &grpcControl{grpcWriter: w,
		heartbeat: func(h *bdsevm.Heartbeat) proto.Message {
			return &bdsevm.StreamLogsResponse{Message: &bdsevm.StreamLogsResponse_Heartbeat{Heartbeat: h}}
		},
		status: func(st *bdsevm.StreamStatus) proto.Message {
			return &bdsevm.StreamLogsResponse{Message: &bdsevm.StreamLogsResponse_Status{Status: st}}
		},
		undo: func(u *bdsevm.Undo) proto.Message {
			return &bdsevm.StreamLogsResponse{Message: &bdsevm.StreamLogsResponse_Undo{Undo: u}}
		},
	}
	return gs.runGrpcSession(ctx, nw, s)
}

// grpcLogsFilter is a request's filter as ws subscribes parse it (CT§11):
// an empty topic position matches any topic.
func grpcLogsFilter(f *bdsevm.LogFilter) (*logfilter.LogsFilter, error) {
	obj := map[string]any{}
	if len(f.GetAddress()) > 0 {
		addrs := make([]any, len(f.Address))
		for i, a := range f.Address {
			addrs[i] = bdsevm.BytesToHex(a)
		}
		obj["address"] = addrs
	}
	if len(f.GetTopics()) > 0 {
		topics := make([]any, len(f.Topics))
		for i, t := range f.Topics {
			if len(t.GetValues()) == 0 {
				continue
			}
			vals := make([]any, len(t.Values))
			for j, v := range t.Values {
				vals[j] = bdsevm.BytesToHex(v)
			}
			topics[i] = vals
		}
		obj["topics"] = topics
	}
	lf, err := logfilter.ParseSubscribe(obj)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid filter: %v", err)
	}
	return lf, nil
}

// grpcLogsEncoder is the StreamLogs encoder: a block's messages, each
// within the send wait. Every block is written: its last message is its
// block end. A block with a message past the bound is never written.
type grpcLogsEncoder struct {
	tracker *evm.ChainTracker
	w       *grpcWriter
}

func (e *grpcLogsEncoder) Block(blk indexer.Block, f *logfilter.LogsFilter, _ bool) (func() error, error) {
	msgs, err := grpcLogsEncoding.Of(e.tracker, blk, f)
	if err != nil {
		return nil, streamEncodeError{blk.Number, err}
	}
	for _, m := range msgs {
		if err := e.w.fits(blk, m); err != nil {
			return nil, err
		}
	}
	return func() error {
		for _, m := range msgs {
			if err := e.w.write(streamData, m); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// grpcLog is one held log as a BlockLogs logs element, encoded.
func grpcLog(raw json.RawMessage) (mem.Buffer, error) {
	var l bdsevm.JsonRpcLog
	if err := common.SonicCfg.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	p, err := l.ToProto()
	if err != nil {
		return nil, err
	}
	return grpcItem(fieldBlockLogsLogs, p)
}

// grpcBlockLogs is a block's StreamLogs messages from its matched logs,
// each already encoded: BlockLogs of grpcBatches runs, the last complete;
// one with no logs for a block with none. It reports the bytes the
// messages hold beyond the logs they share.
func grpcBlockLogs(b indexer.Block, logs []mem.Buffer) ([]*grpcEncoded, int, error) {
	ref, err := proto.Marshal(&bdsevm.BlockRef{Number: b.Number, Hash: b.Hash[:], ParentHash: b.ParentHash[:]})
	if err != nil {
		return nil, 0, err
	}
	head := protowire.AppendBytes(protowire.AppendTag(make([]byte, 0, 2+protowire.SizeBytes(len(ref))), fieldBlockLogsBlock, protowire.BytesType), ref)
	var out []*grpcEncoded
	held := 0
	err = grpcBatches(len(logs), func(i int) int { return logs[i].Len() },
		func(from, to int) error {
			var tail mem.Buffer
			if to == len(logs) {
				tail = blockLogsComplete
			}
			m := grpcMessage(fieldLogsResponseBlock, head, logs[from:to], tail)
			held += m.held()
			out = append(out, m)
			return nil
		})
	return out, held, err
}
