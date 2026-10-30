package erpc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// grpcSendWait: one send blocked this long ends the stream slow_client
// (CT§10.2); a var so tests run fast.
var grpcSendWait = util.NewDuration(10 * time.Second)

var (
	errGrpcSendSlow      = errors.New("the client took no message within the send wait")
	errGrpcSenderStopped = errors.New("the stream ended")
)

// gRPC stream bounds (streams spec §6.2, §10).
const (
	// grpcBatchMax logs or transactions at most in one message.
	grpcBatchMax = 1000
	// grpcBatchBytes bounds a message's logs or transactions, so a message
	// of several stays within 1 MiB; a larger item goes alone.
	grpcBatchBytes = 1<<20 - 4<<10
	// grpcMessageMax is the client default receive limit: a larger message
	// fails at a client that keeps it, whatever the server sends.
	grpcMessageMax = 4 << 20
	// grpcStreamsPerConn long-lived streams at most on one connection.
	grpcStreamsPerConn = 100
)

// grpcErrorDomain is the google.rpc.ErrorInfo domain of every gRPC error
// eRPC details (AIP-193).
const grpcErrorDomain = "erpc.cloud"

// grpcEncoded is a stream message encoded once and sent as is by every
// stream that sends it (§6.2): its buffers, in order. An item (a log, a
// transaction) is a buffer of its own, shared by every message that holds
// it, so no message copies it.
type grpcEncoded struct {
	bufs mem.BufferSlice
	size int
}

func encodeGrpc(m proto.Message) (*grpcEncoded, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &grpcEncoded{bufs: mem.BufferSlice{mem.SliceBuffer(b)}, size: len(b)}, nil
}

// grpcItem is p as one element of the repeated field, tag and length
// included: one buffer, in one allocation, for messages to share.
func grpcItem(field protowire.Number, p proto.Message) (mem.Buffer, error) {
	n := proto.Size(p)
	b := make([]byte, 0, protowire.SizeTag(field)+protowire.SizeVarint(uint64(n))+n)
	b = protowire.AppendTag(b, field, protowire.BytesType)
	b = protowire.AppendVarint(b, uint64(n))
	b, err := proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(b, p)
	if err != nil {
		return nil, err
	}
	return mem.SliceBuffer(b), nil
}

// grpcMessage is a message whose field outer holds head, then items, then
// tail, all already encoded; items are referenced, not copied.
func grpcMessage(outer protowire.Number, head []byte, items []mem.Buffer, tail mem.Buffer) *grpcEncoded {
	n := len(head)
	for _, it := range items {
		n += it.Len()
	}
	if tail != nil {
		n += tail.Len()
	}
	prefix := make([]byte, 0, protowire.SizeTag(outer)+protowire.SizeVarint(uint64(n))+len(head))
	prefix = protowire.AppendTag(prefix, outer, protowire.BytesType)
	prefix = protowire.AppendVarint(prefix, uint64(n))
	prefix = append(prefix, head...)
	bufs := make(mem.BufferSlice, 0, len(items)+2)
	bufs = append(bufs, mem.SliceBuffer(prefix))
	bufs = append(bufs, items...)
	if tail != nil {
		bufs = append(bufs, tail)
	}
	return &grpcEncoded{bufs: bufs, size: len(prefix) + n - len(head)}
}

// held is the bytes m itself holds, beyond the items it shares.
func (m *grpcEncoded) held() int {
	const iface, header = 16, 64 // a buffer in bufs; the struct and slice headers
	return header + iface*cap(m.bufs) + m.bufs[0].Len()
}

// grpcCodec is the server's codec: proto, except that a grpcEncoded
// message goes out as its buffers, with no copy.
type grpcCodec struct{ encoding.CodecV2 }

func newGrpcCodec() grpcCodec { return grpcCodec{encoding.GetCodecV2("proto")} }

func (c grpcCodec) Marshal(v any) (mem.BufferSlice, error) {
	if m, ok := v.(*grpcEncoded); ok {
		return m.bufs, nil
	}
	return c.CodecV2.Marshal(v)
}

// streamSender sends on a stream from its own goroutine, so a client that
// stops reading ends the stream after the send wait instead of holding the
// tap in a blocked Send. Sends from the tap, heartbeat and status
// goroutines go one at a time; after an error every send fails with it.
type streamSender struct {
	mu    sync.Mutex
	err   error
	timer *time.Timer
	msgs  chan *grpcEncoded
	sent  chan error
	done  chan struct{}
	stop  func()
}

// newStreamSender is stream's sender. Its messages go out uncompressed
// whatever the client asks (§6.3): each is encoded once for every
// stream, and compressing it per stream would spend that saving.
func newStreamSender(stream grpc.ServerStream, method string) *streamSender {
	// Fails only without a transport stream in the context, which a
	// server handler's stream always has.
	_ = grpc.SetSendCompressor(stream.Context(), encoding.Identity)
	s := &streamSender{msgs: make(chan *grpcEncoded), sent: make(chan error, 1), done: make(chan struct{})}
	s.stop = sync.OnceFunc(func() { close(s.done) })
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"grpc-stream-send", "method:"+method, common.ErrorFingerprint(rec),
				).Inc()
				s.sent <- fmt.Errorf("send panicked: %v", rec)
			}
		}()
		for {
			select {
			case m := <-s.msgs:
				s.sent <- stream.SendMsg(m)
			case <-s.done:
				return
			}
		}
	}()
	return s
}

// send hands m to the sender and waits up to wait for the stream to take
// it.
func (s *streamSender) send(m *grpcEncoded, wait time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.timer == nil {
		s.timer = time.NewTimer(wait)
	} else {
		s.timer.Reset(wait)
	}
	defer s.timer.Stop()
	select {
	case s.msgs <- m:
	case <-s.timer.C:
		s.err = errGrpcSendSlow
		return s.err
	case <-s.done:
		s.err = errGrpcSenderStopped
		return s.err
	}
	select {
	case s.err = <-s.sent:
	case <-s.timer.C:
		s.err = errGrpcSendSlow
	case <-s.done:
		s.err = errGrpcSenderStopped
	}
	return s.err
}

// grpcWriter writes one gRPC stream's messages within the send wait and
// meters them; max is the server's message bound (§6.2).
type grpcWriter struct {
	sender *streamSender
	wait   time.Duration
	max    int
	meter  *streamMeter
}

// fits is nil when m is within the message bound, else blk's
// messageTooLargeError.
func (w *grpcWriter) fits(blk indexer.Block, m *grpcEncoded) error {
	if m.size <= w.max {
		return nil
	}
	return messageTooLargeError{block: indexer.Cursor{Number: blk.Number, Hash: blk.Hash}, size: m.size, max: w.max}
}

func (w *grpcWriter) write(t streamMsgType, m *grpcEncoded) error {
	if err := w.sender.send(m, w.wait); err != nil {
		return streamWriteError{err: err, slow: errors.Is(err, errGrpcSendSlow)}
	}
	w.meter.wrote(t, m.size)
	return nil
}

func (w *grpcWriter) writeProto(t streamMsgType, m proto.Message) error {
	enc, err := encodeGrpc(m)
	if err != nil {
		return err
	}
	return w.write(t, enc)
}

// grpcControl writes a gRPC stream's control messages (§3.2) as DATA
// messages of its response type, so a proxy that ignores HTTP/2 PING sees
// a heartbeat (§3.6). undo is nil on streams without an undo mode. The end
// is the stream's status, which its handler returns (§3.4).
type grpcControl struct {
	*grpcWriter
	heartbeat func(*bdsevm.Heartbeat) proto.Message
	status    func(*bdsevm.StreamStatus) proto.Message
	undo      func(*bdsevm.Undo) proto.Message
}

func (c *grpcControl) Heartbeat(hb streamHeartbeat) error {
	return c.writeProto(streamHeartbeatMsg, c.heartbeat(&bdsevm.Heartbeat{
		Head:      &bdsevm.BlockRef{Number: hb.head.Number, Hash: hb.head.Hash[:]},
		Finalized: hb.finalized,
		Safe:      hb.safe,
	}))
}

var grpcStates = map[string]bdsevm.StreamStatus_State{
	streamOK:       bdsevm.StreamStatus_STATE_OK,
	streamDegraded: bdsevm.StreamStatus_STATE_DEGRADED,
	streamLagging:  bdsevm.StreamStatus_STATE_LAGGING,
}

func (c *grpcControl) Status(state, reason string) error {
	return c.writeProto(streamStatus, c.status(&bdsevm.StreamStatus{State: grpcStates[state], Reason: reason}))
}

func (c *grpcControl) Undo(forkPoint indexer.Cursor) error {
	if c.undo == nil {
		return errors.New("the stream has no undo mode")
	}
	return c.writeProto(streamUndo, c.undo(&bdsevm.Undo{ForkPoint: &bdsevm.BlockRef{Number: forkPoint.Number, Hash: forkPoint.Hash[:]}}))
}

func (c *grpcControl) End(streamEnd) error { return nil }

// grpcStreamStart is a request's resume cursor (CT§10.3); nil starts after
// the bound.
func grpcStreamStart(fromBlock *uint64, fromHash []byte) (*indexer.Cursor, error) {
	switch {
	case fromBlock == nil && fromHash == nil:
		return nil, nil
	case fromBlock == nil || fromHash == nil:
		return nil, status.Error(codes.InvalidArgument, "fromBlock and fromHash go together: set both to resume, or neither")
	case len(fromHash) != 32:
		return nil, status.Errorf(codes.InvalidArgument, "fromHash has %d bytes, not 32", len(fromHash))
	}
	c := indexer.Cursor{Number: *fromBlock}
	copy(c.Hash[:], fromHash)
	return &c, nil
}

// grpcOptionFields name the §3.1 options as the stream requests do.
var grpcOptionFields = map[string]string{"heartbeat": "heartbeatSeconds", "finality": "finality", "reorg": "reorg", "depth": "depth"}

// grpcStreamOptions are a gRPC stream's options, validated by the §3.1
// rows (parseStreamOptions) from own, keyed by their names there; an
// error is INVALID_ARGUMENT naming the request field.
func grpcStreamOptions(kind string, own map[string]any) (StreamOptions, error) {
	o, err := parseStreamOptions(kind, own, nil)
	return o, grpcOptionError(err)
}

func grpcOptionError(err error) error {
	var oe streamOptionError
	if errors.As(err, &oe) {
		return status.Errorf(codes.InvalidArgument, "invalid %s: %s", grpcOptionFields[oe.name], oe.reason)
	}
	return err
}

// grpcFinality sets own's finality from a request's finality and depth.
func grpcFinality(own map[string]any, f bdsevm.Finality, depth uint32) error {
	if depth != 0 && f != bdsevm.Finality_FINALITY_DEPTH {
		return grpcOptionError(errStreamOption("depth", "set it with FINALITY_DEPTH only"))
	}
	switch f {
	case bdsevm.Finality_FINALITY_LATEST:
		own["finality"] = "latest"
	case bdsevm.Finality_FINALITY_SAFE:
		own["finality"] = "safe"
	case bdsevm.Finality_FINALITY_FINALIZED:
		own["finality"] = "finalized"
	case bdsevm.Finality_FINALITY_DEPTH:
		if depth == 0 {
			return grpcOptionError(errStreamOption("depth", "must be 1 to the reorg window with FINALITY_DEPTH"))
		}
		own["finality"] = "depth:" + strconv.FormatUint(uint64(depth), 10)
	default:
		return grpcOptionError(errStreamOption("finality", fmt.Sprintf("unknown value %d", f)))
	}
	return nil
}

// grpcHeartbeat sets own's heartbeat from a request's heartbeatSeconds.
func grpcHeartbeat(own map[string]any, seconds *uint32) {
	if seconds != nil {
		own["heartbeat"] = float64(*seconds)
	}
}

// openGrpcStream authenticates a stream client as authMethod and takes
// its rate limit permits, once, as eth_subscribe of kind does on ws
// (§6.5); then admits opts on its network.
func (gs *GrpcServer) openGrpcStream(ctx context.Context, authMethod, kind string, opts StreamOptions) (*Network, error) {
	input, err := gs.extractRequestInput(ctx, authMethod)
	if err != nil {
		return nil, err
	}
	project, err := gs.erpc.GetProject(input.ProjectId)
	if err != nil {
		return nil, grpcStatusOf(err)
	}
	nq := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest(MethodEthSubscribe, []interface{}{kind}),
	)
	nq.SetClientIP(input.ClientIP)
	if input.UserAgent != "" {
		nq.SetAgentName(input.UserAgent)
	}
	user, err := project.AuthenticateConsumer(ctx, nq, authMethod, input.AuthPayload)
	if err != nil {
		return nil, grpcStatusOf(err)
	}
	nq.SetUser(user)
	if project.Config != nil && project.Config.TrustUserIdHeader {
		nq.SetUserFromTrustedHeader(input.TrustedUserId)
	}
	nw, err := project.GetNetwork(ctx, input.Architecture+":"+input.ChainId)
	if err != nil {
		return nil, grpcStatusOf(err)
	}
	nq.SetNetwork(nw)
	if err := acquireSubscribeRateLimits(ctx, project, nw, nq); err != nil {
		return nil, grpcStatusOf(err)
	}
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), MethodEthSubscribe,
		nq.Finality(ctx).String(), nq.UserId(), nq.AgentName(),
	).Inc()
	if kind != indexer.SubTypeNewPendingTransactions && nw.chainTracker == nil {
		return nil, status.Errorf(codes.Unimplemented, "%s streams need an evm network", kind)
	}
	if err := nw.admitStream(opts, "depth"); err != nil {
		if errors.As(err, new(finalitySpanError)) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, grpcOptionError(err)
	}
	return nw, nil
}

// runGrpcSession runs a heads or logs session on its stream until it
// ends, and returns the stream's status (§3.4). Every gRPC tap is
// resumable: it ends stalled after the read wait while this instance
// lags. A safe stream waits, at most W, for its first Safe before its
// start is fixed.
func (gs *GrpcServer) runGrpcSession(ctx context.Context, nw *Network, s *streamSession) error {
	// The tap ends with its client or its network, whichever ends first.
	tapCtx, stop := context.WithCancel(nw.appCtx)
	defer stop()
	defer context.AfterFunc(ctx, stop)()
	if s.opts.Finality.Kind == finalitySafe {
		defer nw.openSafe(ctx)()
	}
	if s.opts.From == nil {
		s.start = nw.tapStart(s.opts.Finality, "")
	}
	s.resumable, s.drain = true, gs.appCtx.Done()
	done := make(chan *streamEnd, 1)
	if !nw.bg.Go(func() { done <- s.run(tapCtx) }) {
		return status.Error(codes.Unavailable, "network is stopping")
	}
	return gs.grpcEnd(ctx, tapCtx, nw, s.kind, <-done)
}

// grpcEnd is the status a stream ends with (§3.4): out_of_range is
// OUT_OF_RANGE with an ErrorInfo naming the detail and the oldest block a
// client can resume from; message_too_large RESOURCE_EXHAUSTED with an
// ErrorInfo naming the block and the sizes; final_reverted ABORTED;
// draining, stalled, slow_client and lagging UNAVAILABLE, which a client
// resumes from its cursor.
func (gs *GrpcServer) grpcEnd(ctx, tapCtx context.Context, nw *Network, kind string, e *streamEnd) error {
	var encErr streamEncodeError
	var tooLarge messageTooLargeError
	switch {
	case e != nil && e.reason == indexer.TapEndDraining:
		return status.Error(codes.Unavailable, errStreamDraining.Error())
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case tapCtx.Err() != nil:
		return status.Error(codes.Unavailable, "network is stopping")
	case e == nil:
		return status.Error(codes.Unavailable, "stream ended")
	case e.reason == indexer.TapEndOutOfRange:
		var md map[string]string
		if e.oldest != nil {
			md = map[string]string{
				"oldestBlock": strconv.FormatUint(e.oldest.Number, 10),
				"oldestHash":  "0x" + hex.EncodeToString(e.oldest.Hash[:]),
			}
		}
		return grpcErrorWithInfo(codes.OutOfRange, indexer.OutOfRangeError{Reason: e.detail}.Error(), e.detail, md)
	case e.reason == indexer.TapEndMessageTooLarge && errors.As(e.err, &tooLarge):
		return grpcErrorWithInfo(codes.ResourceExhausted, tooLarge.Error(), indexer.TapEndMessageTooLarge, map[string]string{
			"blockNumber":  strconv.FormatUint(tooLarge.block.Number, 10),
			"blockHash":    "0x" + hex.EncodeToString(tooLarge.block.Hash[:]),
			"messageBytes": strconv.Itoa(tooLarge.size),
			"maxBytes":     strconv.Itoa(tooLarge.max),
		})
	case e.reason == indexer.TapEndFinalReverted:
		return status.Errorf(codes.Aborted, "final_reverted: a reorg reverted block %d 0x%x, which the client applied", e.block.Number, e.block.Hash)
	case e.reason == indexer.TapEndStalled:
		return status.Error(codes.Unavailable, errStreamStalled.Error())
	case e.reason == indexer.TapEndSlowClient:
		return status.Error(codes.Unavailable, errGrpcSendSlow.Error())
	case e.reason == indexer.TapEndLagging:
		return status.Error(codes.Unavailable, errTapLag.Error())
	case errors.As(e.err, &encErr):
		gs.logger.Error().Err(e.err).Str("network", nw.Label()).Str("kind", kind).Msg("gRPC stream cannot encode a held block")
		return status.Error(codes.Internal, e.err.Error())
	default:
		// A tap panic or a failed send: the client resumes from its cursor.
		gs.logger.Debug().Err(e.err).Str("network", nw.Label()).Str("kind", kind).Msg("gRPC stream tap ended")
		return status.Error(codes.Unavailable, fmt.Sprint(e.err))
	}
}

// grpcErrorWithInfo is a status of code and msg with a google.rpc.ErrorInfo
// as AIP-193 has it: reason, a snake_case end reason or detail, in
// UPPER_SNAKE; eRPC's domain; metadata md.
func grpcErrorWithInfo(code codes.Code, msg, reason string, md map[string]string) error {
	st := status.New(code, msg)
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: strings.ToUpper(reason), Domain: grpcErrorDomain, Metadata: md}); err == nil {
		st = d
	}
	return st.Err()
}

// grpcBatches splits n items into runs of at most grpcBatchMax items whose
// sizes sum to at most grpcBatchBytes, an item larger than that alone, and
// calls run on each; n = 0 is one empty run.
func grpcBatches(n int, size func(i int) int, run func(from, to int) error) error {
	for from := 0; ; {
		to, bytes := from, 0
		for to < n && to-from < grpcBatchMax {
			sz := size(to)
			if to > from && bytes+sz > grpcBatchBytes {
				break
			}
			bytes += sz
			to++
		}
		if err := run(from, to); err != nil {
			return err
		}
		if to >= n {
			return nil
		}
		from = to
	}
}

// grpcConnStreams counts each connection's long-lived streams on the
// standalone listener: it tags every connection's context, which its
// streams' contexts derive from. On the port shared with HTTP the HTTP
// server tags it (grpcConnContext).
type grpcConnStreams struct{}

type grpcConnStreamsKey struct{}

// grpcConnContext is a connection's context with its stream count.
func grpcConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, grpcConnStreamsKey{}, new(atomic.Int32))
}

func (grpcConnStreams) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return grpcConnContext(ctx, nil)
}
func (grpcConnStreams) HandleConn(context.Context, stats.ConnStats) {}
func (grpcConnStreams) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (grpcConnStreams) HandleRPC(context.Context, stats.RPCStats) {}

var streamServicePrefix = "/" + bdsevm.StreamService_ServiceDesc.ServiceName + "/"

// streamCap refuses a connection's long-lived stream past
// grpcStreamsPerConn with RESOURCE_EXHAUSTED (§10), counted in
// erpc_grpc_streams_rejected_total.
func (gs *GrpcServer) streamCap() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		n, ok := ss.Context().Value(grpcConnStreamsKey{}).(*atomic.Int32)
		if !ok || !strings.HasPrefix(info.FullMethod, streamServicePrefix) {
			return handler(srv, ss)
		}
		defer n.Add(-1)
		if n.Add(1) > grpcStreamsPerConn {
			telemetry.CounterHandle(telemetry.MetricGrpcStreamsRejectedTotal, gs.projectLabel(ss.Context()), "per_connection").Inc()
			return status.Errorf(codes.ResourceExhausted, "at most %d streams per connection", grpcStreamsPerConn)
		}
		return handler(srv, ss)
	}
}

// projectLabel is the stream's project when it names a known one, else
// "unknown", so a client cannot mint label values.
func (gs *GrpcServer) projectLabel(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	id := firstMD(md, "x-erpc-project")
	if id != "" {
		if _, err := gs.erpc.GetProject(id); err == nil {
			return id
		}
	}
	return "unknown"
}
