package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
}

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
	// fills the whole window while any registered tap is Resumable.
	Want(o ReadOpts) (release func())
}

var ErrOutOfRange = errors.New("cursor out of range")

// Why a tap's cursor went out of range; erpc_tap_out_of_range_total labels.
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
)

// OutOfRangeError is ErrOutOfRange with its reason.
type OutOfRangeError struct{ Reason string }

func (e OutOfRangeError) Error() string        { return ErrOutOfRange.Error() + ": " + e.Reason }
func (e OutOfRangeError) Is(target error) bool { return target == ErrOutOfRange }

// MempoolFeed is the reader side of one network's pending transactions.
type MempoolFeed interface {
	Tail(variant string) uint64 // newest sequence; a new tap starts after it
	// Read waits for payloads after seq, bounded by ctx.
	// ErrOutOfRange: seq left the ring.
	Read(ctx context.Context, variant string, after uint64) (txs []json.RawMessage, next uint64, err error)
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
