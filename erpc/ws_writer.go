package erpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/semaphore"
)

// A ws connection's data messages all go through one writer goroutine
// (writeLoop), in the order they are queued: answers, metering frames and
// notifications. Producers encode outside of it and only queue, so none
// waits on another producer's write, and nothing allocates while a write is
// in progress. Under load a shared write lock queued hundreds of answers
// behind holders parked in GC assists.

const (
	// wsOutBytes bounds the bytes queued beyond what the socket took, so a
	// client that stops reading holds up its taps as soon as the kernel's
	// buffers fill (spec §19 D1), as a blocked write did.
	wsOutBytes = 64 << 10
	// wsOutCap is the most messages queued: each weighs at least
	// wsOutBytes/wsOutCap, so a producer that got its bytes never waits
	// on the channel.
	wsOutCap = 256
)

// errWsSendWait ends a producer that could not queue for wsWriteDeadline.
var errWsSendWait = errors.New("websocket send wait elapsed")

// wsWriter is the state of a connection's writer.
type wsWriter struct {
	out    chan wsOut
	budget *semaphore.Weighted
	// ended is closed at the first failed write, with err set before; the
	// writer then fails every message. endCtx ends with it, or with stop.
	ended         chan struct{}
	err           error
	endCtx        context.Context
	endCancel     context.CancelFunc
	stop, stopped chan struct{}
}

// wsOut is one queued message.
type wsOut struct {
	msg    *bytes.Buffer
	weight int64
	// note is set for a notification, skipped once its subscription is
	// removed; size is its bytes for the sent metrics.
	note *wsNotifier
	size int
	// done, when set, gets the write's outcome.
	done chan error
}

func (wsc *WsConnection) startWriter() {
	w := &wsc.w
	w.out = make(chan wsOut, wsOutCap)
	w.budget = semaphore.NewWeighted(wsOutBytes)
	w.ended = make(chan struct{})
	w.endCtx, w.endCancel = context.WithCancel(context.Background())
	w.stop, w.stopped = make(chan struct{}), make(chan struct{})
	go wsc.writeLoop()
}

// stopWriter lets the writer fail what is still queued (teardown set
// closed first) and waits for it to return.
func (wsc *WsConnection) stopWriter() {
	close(wsc.w.stop)
	<-wsc.w.stopped
}

func (wsc *WsConnection) writeLoop() {
	w := &wsc.w
	defer close(w.stopped)
	defer w.endCancel()
	for {
		select {
		case m := <-w.out:
			wsc.writeOut(m)
		case <-w.stop:
			for {
				select {
				case m := <-w.out:
					wsc.writeOut(m)
				default:
					return
				}
			}
		}
	}
}

func (wsc *WsConnection) writeOut(m wsOut) {
	w := &wsc.w
	err := w.err
	switch {
	case err != nil:
	case wsc.closed.Load():
		err = errWsConnClosed
		wsc.endWrites(err)
	case m.note != nil && m.note.sub.Removed():
	default:
		if err = wsc.writeFrame(m.msg.Bytes()); err != nil {
			wsc.endWrites(err)
		} else if m.note != nil {
			m.note.sent.Inc()
			m.note.bytes.Add(float64(m.size))
		}
	}
	w.budget.Release(m.weight)
	releaseMessage(m.msg)
	if m.done != nil {
		m.done <- err
	}
}

func (wsc *WsConnection) endWrites(err error) {
	w := &wsc.w
	w.err = err
	close(w.ended)
	w.endCancel()
}

// writeFrame writes msg; gorilla's WriteMessage on a server connection
// without compression allocates nothing.
func (wsc *WsConnection) writeFrame(msg []byte) error {
	if err := wsc.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
	defer wsc.conn.SetWriteDeadline(time.Time{})
	return wsc.conn.WriteMessage(websocket.TextMessage, msg)
}

// queue hands m to the writer. A producer that cannot queue waits up to
// wsWriteDeadline, as long as one write may take; it fails at once once the
// connection closed or a write failed. m.msg belongs to the writer after.
func (wsc *WsConnection) queue(m wsOut) error {
	w := &wsc.w
	if wsc.closed.Load() {
		releaseMessage(m.msg)
		return errWsConnClosed
	}
	select {
	case <-w.ended:
		releaseMessage(m.msg)
		return w.err
	default:
	}
	m.weight = min(max(int64(m.msg.Len()), wsOutBytes/wsOutCap), wsOutBytes)
	if !w.budget.TryAcquire(m.weight) {
		ctx, cancel := context.WithTimeout(w.endCtx, wsWriteDeadline)
		err := w.budget.Acquire(ctx, m.weight)
		cancel()
		if err != nil {
			releaseMessage(m.msg)
			select {
			case <-w.ended:
				return w.err
			default:
			}
			if w.endCtx.Err() != nil {
				return errWsConnClosed
			}
			return errWsSendWait
		}
	}
	w.out <- m
	return nil
}

// send queues msg and waits for its write.
func (wsc *WsConnection) send(msg *bytes.Buffer) error {
	done := make(chan error, 1)
	if err := wsc.queue(wsOut{msg: msg, done: done}); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-wsc.w.stopped:
		// Queued after the writer's last look.
		select {
		case err := <-done:
			return err
		default:
			return errWsConnClosed
		}
	}
}

// wsMessages holds message buffers.
var wsMessages = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// wsMessagePooledMax: a larger buffer is dropped after its write, so one
// large response does not stay pinned in the pool.
const wsMessagePooledMax = 64 << 10

func releaseMessage(b *bytes.Buffer) {
	if b.Cap() <= wsMessagePooledMax {
		wsMessages.Put(b)
	}
}

// encodeMessage is one text message produced by write. With cost frames on,
// the message starts with costs (a header block, possibly empty) and the
// blank line ending it, so the block and its body are one message.
func (wsc *WsConnection) encodeMessage(costs []byte, write func(w io.Writer) error) (*bytes.Buffer, error) {
	b := wsMessages.Get().(*bytes.Buffer)
	b.Reset()
	if wsc.costFrames {
		b.Write(costs)
		b.Write(crlf)
	}
	if err := write(b); err != nil {
		releaseMessage(b)
		return nil, err
	}
	return b, nil
}

// writeMessage writes the message write produces and returns the write's
// outcome.
func (wsc *WsConnection) writeMessage(costs []byte, write func(w io.Writer) error) error {
	if wsc.closed.Load() {
		return errWsConnClosed
	}
	msg, err := wsc.encodeMessage(costs, write)
	if err != nil {
		return err
	}
	return wsc.send(msg)
}
