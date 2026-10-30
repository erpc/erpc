package erpc

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsclient"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
)

// logsCloseSpread spreads the 1013 closes of standard logs taps over this
// long, so one event does not make every client reconnect at once
// (CT§10.2); a var so tests run fast.
var logsCloseSpread = util.NewDuration(60 * time.Second)

// newHeadsEncoding is each pinned header's newHeads payload.
var newHeadsEncoding = evm.NewHeaderEncoding("ws", evm.NewHeadsPayload)

// wsTapLoopStart, while set, runs as each ws newHeads or logs tap's loop
// begins; tests hold it there to start the loop late.
var wsTapLoopStart atomic.Pointer[func()]

func wsTapLoopStarts() {
	if hold := wsTapLoopStart.Load(); hold != nil {
		(*hold)()
	}
}

// parseLogsSubscribe parses the filter of eth_subscribe logs params, the
// erpc object already taken off; one that does not parse is -32602.
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

// wsStream is the ws encoder of one subscription (streams spec §5.2):
// data stay eth_subscription notifications; control messages are
// erpc_subscription frames, which standard libraries ignore. Control and
// data go through the connection's one writer in the order they are
// queued, so a blockEnd follows its block's last log.
type wsStream struct {
	sm      *SubscriptionManager
	wsc     *WsConnection
	sub     wsclient.Subscription
	tracker *evm.ChainTracker
	notify  *wsNotifier
	// ctrl is the control envelope up to the result:
	// {"jsonrpc":"2.0","method":"erpc_subscription","params":{"subscription":"<id>","result":
	ctrl []byte
}

func (sm *SubscriptionManager) wsStream(wsc *WsConnection, sub wsclient.Subscription, nw *Network, kind string) *wsStream {
	id, _ := json.Marshal(sub.ClientSubID)
	ctrl := make([]byte, 0, 96+len(id))
	ctrl = append(ctrl, `{"jsonrpc":"2.0","method":"erpc_subscription","params":{"subscription":`...)
	ctrl = append(ctrl, id...)
	ctrl = append(ctrl, `,"result":`...)
	notify := wsc.notifier(sub)
	notify.meter = newStreamMeter(nw, "ws", kind)
	return &wsStream{sm: sm, wsc: wsc, sub: sub, tracker: nw.chainTracker, notify: notify, ctrl: ctrl}
}

func (e *wsStream) data(t streamMsgType, payload json.RawMessage, d tapDelivery) error {
	if err := e.notify.write(t, payload, d); err != nil {
		return writeFailed(err)
	}
	return nil
}

func (e *wsStream) Header(blk indexer.Block) (func(tapDelivery) error, bool, error) {
	payload, pinned, err := newHeadsEncoding.Of(e.tracker, blk)
	if err != nil {
		e.sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot project a held header as newHeads")
		return nil, pinned, nil
	}
	return func(d tapDelivery) error { return e.data(streamData, payload, d) }, pinned, nil
}

// Block writes blk's matched logs, each an eth_subscription notification,
// then with end its blockEnd; the block is delivered with its last message.
func (e *wsStream) Block(blk indexer.Block, f *logfilter.LogsFilter, end bool) (func(tapDelivery) error, error) {
	logs := e.tracker.MatchedLogs(blk, f)
	if len(logs) == 0 && !end {
		return nil, nil
	}
	return func(d tapDelivery) error {
		logsDelivery := d
		if end {
			logsDelivery = tapDelivery{}
		}
		for i, l := range logs {
			var last tapDelivery
			if i == len(logs)-1 {
				last = logsDelivery
			}
			if err := e.data(streamData, l, last); err != nil {
				return err
			}
		}
		if end {
			return e.write(streamBlockEnd, false, d, func(b []byte) []byte { return appendBlockEnd(b, "blockEnd", blk, len(logs)) })
		}
		return nil
	}, nil
}

func (e *wsStream) Removed(blk indexer.Block, logs []json.RawMessage) error {
	for i := len(logs) - 1; i >= 0; i-- {
		removed, err := removedLog(logs[i])
		if err != nil {
			e.sm.logger.Warn().Err(err).Uint64("block", blk.Number).Msg("cannot mark a held log removed")
			continue
		}
		if err := e.data(streamRemoved, removed, tapDelivery{}); err != nil {
			return err
		}
	}
	return nil
}

func (e *wsStream) Heartbeat(hb streamHeartbeat) error {
	return e.write(streamHeartbeatMsg, false, tapDelivery{}, func(b []byte) []byte { return appendHeartbeat(b, "heartbeat", hb) })
}

func (e *wsStream) Status(state, reason string) error {
	return e.write(streamStatus, false, tapDelivery{}, func(b []byte) []byte { return appendStatus(b, "status", state, reason) })
}

func (e *wsStream) Undo(forkPoint indexer.Cursor) error {
	return e.write(streamUndo, false, tapDelivery{}, func(b []byte) []byte { return appendUndo(b, "undo", forkPoint) })
}

// End writes the end as the subscription's last message, then removes
// the subscription; the connection stays (§5.2).
func (e *wsStream) End(end streamEnd) error {
	err := e.write(streamEndMsg, true, tapDelivery{}, func(b []byte) []byte { return appendEnd(b, "end", end) })
	e.sm.endSubscription(e.wsc, e.sub.ClientSubID)
	return err
}

// write queues one erpc_subscription frame whose result body appends;
// last: nothing of the subscription follows it, and write waits until the
// writer wrote or skipped it.
func (e *wsStream) write(t streamMsgType, last bool, d tapDelivery, body func([]byte) []byte) error {
	if e.sub.Removed() {
		return nil
	}
	msg := body(append(make([]byte, 0, len(e.ctrl)+192), e.ctrl...))
	msg = append(msg, wsNotificationSuffix...)
	buf, err := e.wsc.encodeMessage(nil, func(w io.Writer) error {
		_, err := w.Write(msg)
		return err
	})
	if err != nil {
		return writeFailed(err)
	}
	out := wsOut{msg: buf, note: e.notify, typ: t, last: last, size: len(msg), delivery: d}
	if last {
		err = e.wsc.send(out)
	} else {
		err = e.wsc.queue(out)
	}
	if err != nil {
		return writeFailed(err)
	}
	return nil
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

// runWsTap is one ws newHeads or logs subscription: a stream session on
// the network-wide tracker that writes each applied block, oldest first,
// from head, fixed at subscribe, or from opts.From; it writes only once
// opened, after the client has its id. lane, when set, holds blocks above
// the connection's lane's Latest. A reorg sends newHeads the new branch's
// heads, and logs the removed:true logs of the blocks the client applied.
//
// An extended subscription (§5) ends with an end message and leaves the
// connection open. A standard one (§5.4) keeps CT§10.2: newHeads restarts
// after the head at any end, so the client sees a jump in numbers, as
// with geth; logs close the connection with 1013 after a random delay
// within logsCloseSpread, so the client reconnects knowing it may have
// missed logs. A write that spends the write bound closes the connection.
func (sm *SubscriptionManager) runWsTap(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{},
	nw *Network, lane, kind string, head func(context.Context) (indexer.Cursor, error), hold *indexer.FinalHold, f *logfilter.LogsFilter, opts StreamOptions) {
	wsTapLoopStarts()
	enc := sm.wsStream(wsc, sub, nw, kind)
	s := newStreamSession(nw, "ws", kind, lane, opts)
	s.start, s.hold, s.filter, s.opened, s.gone, s.lag, s.resumable = head, hold, f, opened, wsc.done, true, opts.From != nil
	s.headers, s.logs, s.removed = enc, enc, enc
	if opts.Extended {
		s.control, s.drain = enc, wsc.draining
	}
	for {
		e := s.run(ctx)
		switch {
		case e == nil:
			return
		case e.reason == indexer.TapEndSlowClient:
			wsc.stop(websocket.CloseGoingAway, "slow client", 0)
			return
		case opts.Extended:
			return
		case kind == indexer.SubTypeLogs:
			sm.logger.Warn().Err(e.err).Str("reason", e.reason).Str("detail", e.detail).Str("clientSubId", sub.ClientSubID).
				Msg("logs tap ended, closing the connection with 1013")
			if spread := logsCloseSpread.Load(); spread > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(rand.N(spread)):
				}
			}
			wsc.stop(websocket.CloseTryAgainLater, "logs subscription out of range", 0)
			return
		}
		s.start, s.delivered = nil, false
		if e.reason != "" {
			continue
		}
		sm.logger.Warn().Err(e.err).Str("clientSubId", sub.ClientSubID).Msg("newHeads tap ended, restarting after the head")
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRestartDelay):
		}
	}
}

// runPendingTxs is one ws newPendingTransactions subscription
// (runPendingTap): it writes each transaction as a notification, only once
// opened, after the client has its id. An extended one gets heartbeats
// and ends draining at shutdown (§3.1: heartbeat only).
func (sm *SubscriptionManager) runPendingTxs(ctx context.Context, wsc *WsConnection, sub wsclient.Subscription, opened <-chan struct{}, nw *Network, variant string, from uint64, opts StreamOptions) {
	enc := sm.wsStream(wsc, sub, nw, indexer.SubTypeNewPendingTransactions)
	var beat func(streamHeartbeat) error
	var drain <-chan struct{}
	if opts.Extended {
		beat, drain = enc.Heartbeat, wsc.draining
	}
	e := runPendingTap(ctx, nw, "ws", variant, from, opened, drain, opts, beat, func(txs []*indexer.PendingTx) error {
		for _, tx := range txs {
			if err := enc.data(streamData, tx.Raw, tapDelivery{}); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case e == nil:
	case e.reason == indexer.TapEndDraining:
		if streamOpened(wsc.done, opened) {
			_ = enc.End(*e)
		}
	case e.reason == indexer.TapEndSlowClient:
		wsc.stop(websocket.CloseGoingAway, "slow client", 0)
	}
}

// streamMsgType is a stream message's type label (§13).
type streamMsgType uint8

const (
	streamData streamMsgType = iota
	streamRemoved
	streamUndo
	streamBlockEnd
	streamHeartbeatMsg
	streamStatus
	streamEndMsg
	streamMsgTypes
)

var streamMsgTypeNames = [streamMsgTypes]string{"data", "removed", "undo", "block_end", "heartbeat", "status", "end"}

// streamMeter counts one stream's messages by type and its bytes; each
// series binds on the stream's first message of it.
type streamMeter struct {
	labels []string // project, network, transport, kind
	msgs   [streamMsgTypes]atomic.Pointer[telemetry.BoundCounter]
	bytes  atomic.Pointer[telemetry.BoundCounter]
}

func newStreamMeter(nw *Network, transport, kind string) *streamMeter {
	return &streamMeter{labels: []string{nw.projectId, nw.Label(), transport, kind}}
}

func (m *streamMeter) wrote(t streamMsgType, n int) {
	c := m.msgs[t].Load()
	if c == nil {
		c = telemetry.BindCounter(telemetry.MetricStreamMessagesTotal, append(m.labels[:len(m.labels):len(m.labels)], streamMsgTypeNames[t])...)
		m.msgs[t].Store(c)
	}
	c.Inc()
	b := m.bytes.Load()
	if b == nil {
		b = telemetry.BindCounter(telemetry.MetricStreamBytesTotal, m.labels...)
		m.bytes.Store(b)
	}
	b.Add(float64(n))
}
