package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// streamBlocksSendWait: one Send blocked this long ends the stream
// slow_client (CT§10.2); a var so tests run fast.
var streamBlocksSendWait = util.NewDuration(10 * time.Second)

var errStreamBlocksSlow = errors.New("the client took no block within the send wait")

// streamBlocksAuthMethod is the method consumer auth checks a StreamBlocks
// open against: the stream serves blocks by number, so allowlists that
// name eth_getBlockByNumber admit it.
const streamBlocksAuthMethod = "eth_getBlockByNumber"

// streamBlocksEncoding is each pinned header's StreamBlocks message.
var streamBlocksEncoding = evm.NewHeaderEncoding("grpc", streamBlocksResponse)

// streamEncodeError is a held header the stream cannot encode: sending
// past it would break the parentHash linkage the client relies on.
type streamEncodeError struct {
	number uint64
	err    error
}

func (e streamEncodeError) Error() string {
	return fmt.Sprintf("cannot encode block %d: %v", e.number, e.err)
}

// StreamBlocks is a stream session on the network-wide chain tracker
// (CT§10.2, §10.3; streams spec §2): it sends each applied block, oldest
// first. After a reorg the next block sent is the fork point + 1;
// reverted blocks are never sent, nor is a block that left the pins before
// its send. Every StreamBlocks tap is resumable: it ends stalled after the
// read wait while this instance lags.
// Auth and rate limits run once, at open: auth as eth_getBlockByNumber,
// rate limits as for ws newHeads subscriptions.
func (gs *GrpcServer) StreamBlocks(req *bdsevm.StreamBlocksRequest, stream bdsevm.StreamService_StreamBlocksServer) error {
	ctx := stream.Context()
	start, err := streamBlocksStart(req)
	if err != nil {
		return err
	}
	input, err := gs.extractRequestInput(ctx, streamBlocksAuthMethod)
	if err != nil {
		return err
	}
	nw, err := gs.openBlockStream(ctx, input)
	if err != nil {
		return grpcStatusOf(err)
	}
	if nw.chainTracker == nil {
		return status.Error(codes.Unimplemented, "StreamBlocks needs an evm network")
	}

	// The tap ends with its client or its network, whichever ends first.
	tapCtx, stop := context.WithCancel(nw.appCtx)
	defer stop()
	defer context.AfterFunc(ctx, stop)()
	sender := newStreamSender(stream)
	defer sender.stop()
	s := newStreamSession(nw, "grpc", indexer.SubTypeNewHeads, "", StreamOptions{From: start})
	s.headers = &grpcBlocksEncoder{tracker: nw.chainTracker, sender: sender, wait: streamBlocksSendWait.Load(),
		meter: newStreamMeter(nw, "grpc", indexer.SubTypeNewHeads)}
	s.resumable = true
	done := make(chan *streamEnd, 1)
	if !nw.bg.Go(func() { done <- s.run(tapCtx) }) {
		return status.Error(codes.Unavailable, "network is stopping")
	}
	e := <-done

	var encErr streamEncodeError
	switch {
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case tapCtx.Err() != nil:
		return status.Error(codes.Unavailable, "network is stopping")
	case e == nil:
		return status.Error(codes.Unavailable, "stream ended")
	case e.reason == indexer.TapEndOutOfRange:
		return status.Error(codes.OutOfRange, indexer.OutOfRangeError{Reason: e.detail}.Error())
	case e.reason == indexer.TapEndStalled:
		return status.Error(codes.Unavailable, errStreamStalled.Error())
	case e.reason == indexer.TapEndSlowClient:
		return status.Error(codes.Unavailable, errStreamBlocksSlow.Error())
	case errors.As(e.err, &encErr):
		gs.logger.Error().Err(e.err).Str("network", nw.Label()).Msg("StreamBlocks cannot encode a held header")
		return status.Error(codes.Internal, e.err.Error())
	default:
		// A tap panic or a failed send: the client resumes from its last
		// header.
		gs.logger.Debug().Err(e.err).Str("network", nw.Label()).Msg("StreamBlocks tap ended")
		return status.Error(codes.Unavailable, fmt.Sprint(e.err))
	}
}

// grpcBlocksEncoder is the StreamBlocks encoder: one header message per
// block, sent within the send wait.
type grpcBlocksEncoder struct {
	tracker *evm.ChainTracker
	sender  *streamSender
	wait    time.Duration
	meter   *streamMeter
}

func (e *grpcBlocksEncoder) Header(blk indexer.Block) (func() error, bool, error) {
	resp, pinned, err := streamBlocksEncoding.Of(e.tracker, blk)
	if err != nil {
		return nil, pinned, streamEncodeError{blk.Number, err}
	}
	return func() error {
		if err := e.sender.send(resp, e.wait); err != nil {
			return streamWriteError{err: err, slow: errors.Is(err, errStreamBlocksSlow)}
		}
		e.meter.wrote(streamData, proto.Size(resp))
		return nil
	}, pinned, nil
}

// streamBlocksStart is a request's resume cursor (CT§10.3); nil starts
// after the head.
func streamBlocksStart(req *bdsevm.StreamBlocksRequest) (*indexer.Cursor, error) {
	switch {
	case req.FromBlock == nil && req.FromHash == nil:
		return nil, nil
	case req.FromBlock == nil || req.FromHash == nil:
		return nil, status.Error(codes.InvalidArgument, "fromBlock and fromHash go together: set both to resume, or neither")
	case len(req.FromHash) != 32:
		return nil, status.Errorf(codes.InvalidArgument, "fromHash has %d bytes, not 32", len(req.FromHash))
	}
	c := indexer.Cursor{Number: *req.FromBlock}
	copy(c.Hash[:], req.FromHash)
	return &c, nil
}

// openBlockStream authenticates a StreamBlocks client and takes its rate
// limit permits, once, as eth_subscribe newHeads does on ws.
func (gs *GrpcServer) openBlockStream(ctx context.Context, input *RequestInput) (*Network, error) {
	project, err := gs.erpc.GetProject(input.ProjectId)
	if err != nil {
		return nil, err
	}
	nq := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest(MethodEthSubscribe, []interface{}{indexer.SubTypeNewHeads}),
	)
	nq.SetClientIP(input.ClientIP)
	if input.UserAgent != "" {
		nq.SetAgentName(input.UserAgent)
	}
	user, err := project.AuthenticateConsumer(ctx, nq, streamBlocksAuthMethod, input.AuthPayload)
	if err != nil {
		return nil, err
	}
	nq.SetUser(user)
	if project.Config != nil && project.Config.TrustUserIdHeader {
		nq.SetUserFromTrustedHeader(input.TrustedUserId)
	}
	nw, err := project.GetNetwork(ctx, input.Architecture+":"+input.ChainId)
	if err != nil {
		return nil, err
	}
	nq.SetNetwork(nw)
	if err := acquireSubscribeRateLimits(ctx, project, nw, nq); err != nil {
		return nil, err
	}
	telemetry.CounterHandle(telemetry.MetricNetworkRequestsReceived,
		project.Config.Id, nw.Label(), MethodEthSubscribe,
		nq.Finality(ctx).String(), nq.UserId(), nq.AgentName(),
	).Inc()
	return nw, nil
}

// streamBlocksResponse is the StreamBlocks message of a held header.
func streamBlocksResponse(header json.RawMessage) (*bdsevm.StreamBlocksResponse, error) {
	h, err := evm.StreamBlockHeader(header)
	if err != nil {
		return nil, err
	}
	return &bdsevm.StreamBlocksResponse{Header: h}, nil
}

// streamSender sends on a stream from its own goroutine, so a client that
// stops reading ends the stream after the send wait instead of holding
// the tap in a blocked Send.
type streamSender struct {
	msgs  chan *bdsevm.StreamBlocksResponse
	sent  chan error
	timer *time.Timer
}

func newStreamSender(stream bdsevm.StreamService_StreamBlocksServer) *streamSender {
	s := &streamSender{msgs: make(chan *bdsevm.StreamBlocksResponse), sent: make(chan error, 1)}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"grpc-stream-send", "method:StreamBlocks", common.ErrorFingerprint(rec),
				).Inc()
				s.sent <- fmt.Errorf("send panicked: %v", rec)
			}
		}()
		for m := range s.msgs {
			s.sent <- stream.Send(m)
		}
	}()
	return s
}

// send hands m to the sender and waits up to wait for the stream to take
// it. After an error the sender takes nothing more.
func (s *streamSender) send(m *bdsevm.StreamBlocksResponse, wait time.Duration) error {
	s.msgs <- m
	if s.timer == nil {
		s.timer = time.NewTimer(wait)
	} else {
		s.timer.Reset(wait)
	}
	select {
	case err := <-s.sent:
		s.timer.Stop()
		return err
	case <-s.timer.C:
		return errStreamBlocksSlow
	}
}

// stop ends the sender once its send in flight, if any, returns: the
// stream ends with the handler, which unblocks it.
func (s *streamSender) stop() { close(s.msgs) }
