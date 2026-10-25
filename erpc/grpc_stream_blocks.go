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
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Wait bounds of a StreamBlocks stream (spec §9.2); vars so tests run fast.
var (
	streamBlocksReadWait = util.NewDuration(60 * time.Second)
	streamBlocksSendWait = util.NewDuration(10 * time.Second)
)

var (
	errStreamBlocksStalled = errors.New("no block within the read wait")
	errStreamBlocksSlow    = errors.New("the client took no block within the send wait")
)

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

// StreamBlocks is one tap on the network-wide chain tracker (spec §9.2,
// §9.3): it sends each applied block, oldest first. After a reorg the
// next block sent is the fork point + 1; reverted blocks are never sent,
// nor is a block that left the pins before its send.
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
	tracker := nw.chainTracker
	feed := waitBoundedFeed{ChainTracker: tracker, wait: streamBlocksReadWait.Load(), lastVote: nw.newestHashVoteAt}
	sends := newTapSends(nw, "", "grpc", indexer.SubTypeNewHeads)
	sendWait := streamBlocksSendWait.Load()
	emit := func(b indexer.Batch) (int, error) {
		sends.batch()
		for i, blk := range b.Applied {
			resp, pinned, err := streamBlocksEncoding.Of(tracker, blk)
			if !pinned || !sends.sendable(blk) {
				return i, nil
			}
			if err != nil {
				return i, streamEncodeError{blk.Number, err}
			}
			if err := sends.write(blk, func() error { return sender.send(resp, sendWait) }); err != nil {
				return i, err
			}
		}
		return len(b.Applied), nil
	}
	done := make(chan error, 1)
	if !nw.bg.Go(func() {
		done <- nw.taps.RunChain(tapCtx, nw.Label(), "grpc", indexer.SubTypeNewHeads, feed, start, indexer.ReadOpts{Resumable: true}, emit)
	}) {
		return status.Error(codes.Unavailable, "network is stopping")
	}
	err = <-done

	var encErr streamEncodeError
	switch {
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case tapCtx.Err() != nil:
		return status.Error(codes.Unavailable, "network is stopping")
	case errors.Is(err, indexer.ErrOutOfRange):
		ev := common.TransitionLog(gs.logger, zerolog.InfoLevel, nw.Label(), "out_of_range").Err(err)
		if start != nil {
			ev = ev.Uint64("fromBlock", start.Number).Hex("fromHash", start.Hash[:])
		}
		ev.Msg("StreamBlocks out of range")
		return status.Error(codes.OutOfRange, err.Error())
	case errors.As(err, &encErr):
		gs.logger.Error().Err(err).Str("network", nw.Label()).Msg("StreamBlocks cannot encode a held header")
		return status.Error(codes.Internal, err.Error())
	default:
		// A wait bound, a failed send or a tap panic: the client resumes
		// from its last header.
		gs.logger.Debug().Err(err).Str("network", nw.Label()).Msg("StreamBlocks tap ended")
		return status.Error(codes.Unavailable, err.Error())
	}
}

// streamBlocksStart is a request's resume cursor (spec §9.3); nil starts
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

// waitBoundedFeed ends a wait the client should not sit through with
// errStreamBlocksStalled, so a pod that lags, restarted or fills slowly
// tells the client to reconnect (spec §10.2): a Head wait longer than
// wait, and a Read wait that spends wait while the confirmed head is not the
// cursor and stands at or above it, or while no hash voter exists. A Read
// at the head of a chain that makes no blocks waits without end: no lag
// reference moves, so its hash voters stay.
type waitBoundedFeed struct {
	*evm.ChainTracker
	wait time.Duration
	// lastVote is when the newest hash vote reached this instance; zero
	// before one.
	lastVote func() time.Time
}

func (f waitBoundedFeed) Head(ctx context.Context) (indexer.Cursor, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, f.wait, errStreamBlocksStalled)
	defer cancel()
	c, err := f.ChainTracker.Head(ctx)
	if err != nil && errors.Is(context.Cause(ctx), errStreamBlocksStalled) {
		return c, errStreamBlocksStalled
	}
	return c, err
}

func (f waitBoundedFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go f.watchStall(rctx, c, cancel)
	b, err := f.ChainTracker.Read(rctx, c, o)
	if err != nil && context.Cause(rctx) == errStreamBlocksStalled && ctx.Err() == nil {
		return indexer.Batch{}, errStreamBlocksStalled
	}
	return b, err
}

// watchStall ends a Read on c with errStreamBlocksStalled once it waited
// f.wait in a row while stalled; it is checked on each published state
// and every quarter of the wait. Without a hash voter the wait counts from
// the newest hash vote: a cut-off pod's sockets close only at their
// liveness deadline, well after its last header.
func (f waitBoundedFeed) watchStall(ctx context.Context, c indexer.Cursor, cancel context.CancelCauseFunc) {
	recheck := f.wait / 4
	timer := time.NewTimer(recheck)
	defer timer.Stop()
	var since time.Time
	for {
		changed := f.Changed()
		next := recheck
		voting := f.HashVoting()
		if f.stalled(c, voting) {
			if since.IsZero() {
				since = time.Now()
				if at := f.lastVote(); !voting && !at.IsZero() && at.Before(since) {
					since = at
				}
			}
			left := f.wait - time.Since(since)
			if left <= 0 {
				cancel(errStreamBlocksStalled)
				return
			}
			next = min(next, left)
		} else {
			since = time.Time{}
		}
		timer.Reset(next)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-timer.C:
		}
	}
}

// stalled reports whether a Read on c waits on this pod rather than on the
// chain: no hash voter exists (voting false), or the confirmed head stands
// above c, or at c's number on another branch.
func (f waitBoundedFeed) stalled(c indexer.Cursor, voting bool) bool {
	if !voting {
		return true
	}
	send, ok := f.Confirmed()
	return ok && (send.Number > c.Number || send.Number == c.Number && send.Hash != c.Hash)
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
