package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// ws logs wait bounds (spec §9.2); variables so tests run fast.
var (
	// logsReceiptsWait: a logs tap that waits this long on one block's
	// receipts is out of range.
	logsReceiptsWait = util.NewDuration(60 * time.Second)
	// logsCloseSpread spreads the 1013 closes of logs taps over this long,
	// so one event does not make every client reconnect at once.
	logsCloseSpread = util.NewDuration(60 * time.Second)
)

var (
	errLogsLag             = indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}
	errLogsReorgDepth      = indexer.OutOfRangeError{Reason: indexer.OutOfRangeReorgDepth}
	errLogsReceiptsTimeout = indexer.OutOfRangeError{Reason: indexer.OutOfRangeReceiptsTimeout}
)

// parseLogsSubscribe parses the filter of an eth_subscribe logs request;
// one that does not parse is -32602.
func parseLogsSubscribe(params []interface{}) (*logfilter.LogsFilter, error) {
	var filter any
	if len(params) > 1 {
		filter = params[1]
	}
	f, err := logfilter.ParseSubscribe(filter)
	if err != nil {
		return nil, common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorInvalidArgument,
			"invalid logs filter: "+err.Error(), nil, nil)
	}
	return f, nil
}

// runLogs is one ws logs subscription: a tap on the network-wide tracker
// that reads the receipts of each block the filter's bloom test may match.
// For a reorg it writes the Reverted blocks' matched logs, newest first,
// with removed:true, then the logs of the Applied blocks; an Applied block
// that left the pins before its logs are written ends the batch there. Any
// end other than ctx or a failed write closes the connection with 1013
// after a random delay within logsCloseSpread, so the client reconnects
// knowing it may have missed logs. lane, when set, holds blocks above the
// connection's lane's Latest.
func (sm *SubscriptionManager) runLogs(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, lane string, f *logfilter.LogsFilter) {
	tracker := nw.chainTracker
	sends := newTapSends(nw, lane, "ws", indexer.SubTypeLogs)
	sends := newTapSends(nw, lane, "ws", indexer.SubTypeLogs)
	write := func(log json.RawMessage) error {
		if err := wsc.WriteSubscriptionNotification(sub, log); err != nil {
			return wsWriteError{err}
		}
		return nil
	}
	// sentFrom is the first block the tap applied; the tap's start cursor
	// and anything below it were never sent, so a reorg reverting them
	// sends no removed:true for them (spec §13.3: every removed log is a
	// sent log). sentHashes is the hash the tap applied at each number,
	// within the window below the newest.
	// within the window below the newest.
	sentHashes := make(map[uint64][32]byte, window+1)
	emit := func(b indexer.Batch) (int, error) {
	sentHashes := make(map[uint64][32]byte, window+1)
	emit := func(b indexer.Batch) (int, error) {
	sentHashes := make(map[uint64][32]byte, window+1)
	emit := func(b indexer.Batch) (int, error) {
			return 0, ctx.Err()
	emit := func(b indexer.Batch) (int, error) {
			return 0, ctx.Err()
		select {
			return 0, ctx.Err()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		// removed:true logs need the sent Reverted blocks' receipts, which
		// the feed holds only reorgWindow deep.
		if sent {
			n := 0
				return 0, errLogsReorgDepth
				if blk.Number >= sentFrom {
				return 0, errLogsReorgDepth
				}
			return 0, errLogsLag
			if n > window {
			return 0, errLogsLag
			}
			return 0, errLogsLag
		if len(b.Applied) > 0 && lagging(tracker, b.Applied[0]) {
			if h, ok := sentHashes[blk.Number]; !ok || h != blk.Hash {
				common.ViolateInvariant(nw.logger, nw.projectId, nw.Label(), common.InvariantRemovedUnsent).
					Str("lane", lane).Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("sentHash", h[:]).
					Msg("logs tap reverts a block it never applied; it sends no removed:true for it")
				continue
			}
			delete(sentHashes, blk.Number)
			return 0, errLogsLag
		}
			if h, ok := sentHashes[blk.Number]; !ok || h != blk.Hash {
				common.ViolateInvariant(nw.logger, nw.projectId, nw.Label(), common.InvariantRemovedUnsent).
					Str("lane", lane).Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("sentHash", h[:]).
					Msg("logs tap reverts a block it never applied; it sends no removed:true for it")
				continue
			}
					return 0, err
		for _, blk := range b.Reverted {
			if !sent || blk.Number < sentFrom {
			if h, ok := sentHashes[blk.Number]; !ok || h != blk.Hash {
		for i, blk := range b.Applied {
			if !tracker.Pinned(indexer.Cursor{Number: blk.Number, Hash: blk.Hash}) || !sends.sendable(blk) {
				return i, nil
			}
					return 0, err
				continue
			}
			sentHashes[blk.Number] = blk.Hash
			if blk.Number > uint64(window) {
				delete(sentHashes, blk.Number-uint64(window)-1)
			}
			logs := tracker.MatchedLogs(blk, f)
			if len(logs) == 0 {
				continue
			}
			if err := sends.write(blk, func() error {
				for _, log := range logs {
					if err := write(log); err != nil {
						return err
					}
				return i, nil
				return nil
			}); err != nil {
				return i, err
			}
					return 0, err
		return len(b.Applied), nil
			for i := len(logs) - 1; i >= 0; i-- {
	err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeLogs, gated(logsFeed{tracker}, nw, lane), nil,
			if blk.Number > uint64(window) {
				delete(sentHashes, blk.Number-uint64(window)-1)
			}
			logs := tracker.MatchedLogs(blk, f)
			if len(logs) == 0 {
	common.TransitionLog(sm.logger, zerolog.WarnLevel, nw.Label(), "out_of_range:ws:logs").Err(err).
		Bool("outOfRange", errors.Is(err, indexer.ErrOutOfRange)).Str("clientSubId", sub.ClientSubID).
		Msg("logs tap ended, closing the connection with 1013")
			}
			if err := sends.write(blk, func() error {
				for _, log := range logs {
					if err := write(log); err != nil {
						return err
					}
				return i, nil
				return nil
			}); err != nil {
				return i, err
				if err := write(removed); err != nil {
					return 0, err
		return len(b.Applied), nil
			}
	err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeLogs, gated(logsFeed{tracker}, nw, lane), nil,
			if blk.Number > uint64(window) {
				delete(sentHashes, blk.Number-uint64(window)-1)
			}
			logs := tracker.MatchedLogs(blk, f)
			if len(logs) == 0 {
				continue
			}
			if err := sends.write(blk, func() error {
				for _, log := range logs {
					if err := write(log); err != nil {
						return err
					}
				return i, nil
				return nil
			}); err != nil {
				return i, err
			}
			if !sent || blk.Number < sentFrom {
		return len(b.Applied), nil
			}
	err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeLogs, gated(logsFeed{tracker}, nw, lane), nil,
			if blk.Number > uint64(window) {
				delete(sentHashes, blk.Number-uint64(window)-1)
			}
			logs := tracker.MatchedLogs(blk, f)
			if len(logs) == 0 {
				continue
			}
			if err := sends.write(blk, func() error {
				for _, log := range logs {
					if err := write(log); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return i, err
			}
		}
		return len(b.Applied), nil
	}
	err := nw.taps.RunChain(ctx, nw.Label(), "ws", indexer.SubTypeLogs, gated(logsFeed{tracker}, nw, lane), nil,
		indexer.ReadOpts{MayMatch: f.MayMatch}, emit)
	var writeErr wsWriteError
	if ctx.Err() != nil || errors.As(err, &writeErr) {
		return
	}
	sm.logger.Warn().Err(err).Str("clientSubId", sub.ClientSubID).Msg("logs tap ended, closing the connection with 1013")
	if spread := logsCloseSpread.Load(); spread > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(rand.N(spread)):
		}
	}
	wsc.stop(websocket.CloseTryAgainLater, "logs subscription out of range", 0)
}

// removedLog is log with removed:true, as geth sends a log of a block a
// reorg dropped.
func removedLog(log json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := common.SonicCfg.Unmarshal(log, &m); err != nil {
		return nil, err
	}
	m["removed"] = json.RawMessage("true")
	return common.SonicCfg.Marshal(m)
}

// logsFeed is the network-wide tracker as a logs tap reads it: a Read that
// waits logsReceiptsWait on one block's receipts is out of range. The wait
// counts from when the confirmed head first stands above the cursor, so a
// wait for the next head never counts.
type logsFeed struct{ *evm.ChainTracker }

func (f logsFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go f.watchReceipts(rctx, c, cancel)
	b, err := f.ChainTracker.Read(rctx, c, o)
	if err != nil && context.Cause(rctx) == error(errLogsReceiptsTimeout) && ctx.Err() == nil {
		return indexer.Batch{}, errLogsReceiptsTimeout
	}
	return b, err
}

func (f logsFeed) watchReceipts(ctx context.Context, c indexer.Cursor, cancel context.CancelCauseFunc) {
	var timeout <-chan time.Time
	for {
		changed := f.Changed()
		if timeout == nil {
			if send, ok := f.Confirmed(); ok && send.Number > c.Number {
				t := time.NewTimer(logsReceiptsWait.Load())
				defer t.Stop()
				timeout = t.C
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-timeout:
			cancel(errLogsReceiptsTimeout)
			return
		}
	}
}
