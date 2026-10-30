package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cursor is the number and hash of the last block a tap applied.
type Cursor struct {
	Number uint64
	Hash   [32]byte
}

// Block is one held header, with its receipts when ReadOpts asked for them.
type Block struct {
	Number     uint64
	Hash       [32]byte
	ParentHash [32]byte
	Header     json.RawMessage // verbatim, as first held
	Receipts   json.RawMessage // verbatim; set only for blocks ReadOpts asked for
	// Ready is when the block became deliverable to this Read: when it
	// first reached the confirmed head, or the logs head for a Read with
	// MayMatch. ws taps measure lag from it (spec §10.2).
	Ready time.Time
}

// Batch is what one Read returns.
type Batch struct {
	Reverted []Block // the tap's old branch, newest first
	Applied  []Block // pinned blocks from the fork point + 1, oldest first; at most reorgWindow
	Next     Cursor
}

// ReadOpts selects what a Read returns besides headers.
type ReadOpts struct {
	// MayMatch: Read also returns receipts of each block whose logsBloom
	// may match. Nil: headers only.
	MayMatch func(bloom []byte) bool
	// Resumable: the tap's clients resume from their own cursor, anywhere
	// in the window, so the feed fills the whole window while it is open.
	// Without one, fill stops reorgWindow blocks below the head.
	Resumable bool
	// Pace is how often the tap needs a newer head: 0 every block. The
	// tap's demand carries it (Taps.Pace); a Read ignores it.
	Pace time.Duration
	// Hold: the tap applies no block above Head.Finalized or Head.Safe,
	// so while it is registered the feed also holds every header from
	// reorgWindow below the tap's place up, and fills down to it (streams
	// spec §4). Past the hold's bound its Reads end OutOfRangeFinalitySpan.
	Hold *FinalHold
}

// FinalHold is a final tap's place in its feed's final hold (streams spec
// §4): the lowest block it may still read, its cursor once it has one,
// before that its start. The tap moves it; the feed reads it, and marks
// it spanned once it lies more than the hold's bound below the window.
type FinalHold struct {
	at      atomic.Uint64
	spanned atomic.Bool
}

// Set moves the tap's place to block n; 0 is unknown.
func (h *FinalHold) Set(n uint64) { h.at.Store(n) }

// At is the tap's place, 0 while unknown.
func (h *FinalHold) At() uint64 { return h.at.Load() }

// Span marks the hold past its bound, for good: the tap ends.
func (h *FinalHold) Span() { h.spanned.Store(true) }

// Spanned reports whether the tap ends OutOfRangeFinalitySpan.
func (h *FinalHold) Spanned() bool { return h.spanned.Load() }

// ChainFeed is the reader side of one tracker.
type ChainFeed interface {
	// Head is a new tap's start: the head while a source is live, else the
	// next pin change.
	Head(ctx context.Context) (Cursor, error)
	// Read returns the next non-empty batch from c, waiting as needed,
	// bounded by ctx. ErrOutOfRange: the feed cannot continue c.
	Read(ctx context.Context, c Cursor, o ReadOpts) (Batch, error)
	// Want registers a tap's ReadOpts until release; every tap registers.
	// The feed keeps history while any tap is registered, fetches receipts
	// when a block is pinned and any registered MayMatch wants it, and
	// fills the whole window while any registered tap is Resumable, and
	// from each registered Hold's place up.
	Want(o ReadOpts) (release func())
}

var ErrOutOfRange = errors.New("cursor out of range")

// Why a tap's cursor went out of range; the detail label of
// erpc_tap_end_total.
const (
	OutOfRangeBelowWindow    = "below_window"
	OutOfRangeForkLeftWindow = "fork_left_window"
	OutOfRangeCursorUnknown  = "cursor_unknown"
	// OutOfRangeReorgDepth: a ws logs batch reverts more than reorgWindow
	// blocks, deeper than the feed holds receipts for (spec §10.2).
	OutOfRangeReorgDepth = "reorg_depth"
	// OutOfRangeLag: a transport's wait bound ended the tap (spec §9.2).
	OutOfRangeLag = "lag"
	// OutOfRangeReceiptsTimeout: a logs tap waited too long on one block's
	// receipts (spec §9.2).
	OutOfRangeReceiptsTimeout = "receipts_timeout"
	// OutOfRangeFinalitySpan: a final tap's hold would keep more than its
	// bound of headers below the window (streams spec §4).
	OutOfRangeFinalitySpan = "finality_span"
)

// Why a tap ended; the reason label of erpc_tap_end_total.
const (
	TapEndOutOfRange = "out_of_range"
	TapEndLagging    = "lagging"
	// TapEndStalled: a resumable tap waited the read wait while this
	// instance lags (CT§10.2).
	TapEndStalled = "stalled"
	// TapEndFinalReverted: a reorg none tap saw a block it sent reverted.
	TapEndFinalReverted = "final_reverted"
	// TapEndDraining: the instance shuts down.
	TapEndDraining = "draining"
	// TapEndSlowClient: one write to the client took its write bound.
	TapEndSlowClient = "slow_client"
	// TapEndMessageTooLarge: one item's message passes the transport's
	// message bound (streams spec §6.2); a resume meets it again.
	TapEndMessageTooLarge = "message_too_large"
)

// OutOfRangeError is ErrOutOfRange with its reason.
type OutOfRangeError struct{ Reason string }

func (e OutOfRangeError) Error() string        { return ErrOutOfRange.Error() + ": " + e.Reason }
func (e OutOfRangeError) Is(target error) bool { return target == ErrOutOfRange }

// EndReason is the reason label of erpc_tap_end_total; e.Reason is its detail.
func (e OutOfRangeError) EndReason() string {
	if e.Reason == OutOfRangeLag {
		return TapEndLagging
	}
	return TapEndOutOfRange
}

// MempoolFeed is the reader side of one network's pending transactions.
type MempoolFeed interface {
	Tail(variant string) uint64 // newest sequence; a new tap starts after it
	// Read waits for payloads after seq, bounded by ctx.
	// ErrOutOfRange: seq left the ring.
	Read(ctx context.Context, variant string, after uint64) (txs []*PendingTx, next uint64, err error)
}

// PendingTx is one pending transaction as a feed holds it: Raw is the
// source's payload, verbatim. Encodings holds each transport's encoding of
// it, keyed by the transport, so every tap reading the entry shares one
// conversion (streams spec §6.2); it goes with the entry.
type PendingTx struct {
	Raw       json.RawMessage
	Encodings sync.Map
}

// Topic is what a source feeds: newHeads, or
// newPendingTransactions:<variant>.
type Topic string

const TopicNewHeads Topic = SubTypeNewHeads

const pendingTxTopicPrefix = SubTypeNewPendingTransactions + ":"

// PendingTxTopic is the topic of one pending-transaction variant.
func PendingTxTopic(variant string) Topic { return Topic(pendingTxTopicPrefix + variant) }

// PendingTxVariant is t's variant, if t is a pending-transaction topic.
func (t Topic) PendingTxVariant() (variant string, ok bool) {
	return strings.CutPrefix(string(t), pendingTxTopicPrefix)
}

// PendingTxVariantOf names the variant of newPendingTransactions subscribe
// params: the params after the subscription type, as JSON with sorted keys,
// so equal params from any client are one variant.
func PendingTxVariantOf(params []interface{}) (string, error) {
	rest := []interface{}{}
	if len(params) > 1 {
		rest = params[1:]
	}
	b, err := json.Marshal(rest)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// PendingTxParams are the eth_subscribe params of variant.
func PendingTxParams(variant string) ([]interface{}, error) {
	var rest []interface{}
	if err := json.Unmarshal([]byte(variant), &rest); err != nil {
		return nil, err
	}
	return append([]interface{}{SubTypeNewPendingTransactions}, rest...), nil
}

// Intake is what sources report into; erpc implements it per network.
type Intake interface {
	Header(upstreamId string, raw json.RawMessage)
	PendingTx(upstreamId, variant string, raw json.RawMessage)
	// Subscribed reports a push subscribe's answer for t: nil once it is
	// live, else the upstream's error.
	Subscribed(upstreamId string, t Topic, err error)
}

// Source feeds one upstream's observations over one transport.
type Source interface {
	Set(topics []Topic) // what to feed now; idempotent; empty stops
}
