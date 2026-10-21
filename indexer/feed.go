package indexer

import (
	"context"
	"encoding/json"
	"errors"
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
}

// ChainFeed is the reader side of one tracker.
type ChainFeed interface {
	// Head is a new tap's start: the head while a source is live, else the
	// next pin change.
	Head(ctx context.Context) (Cursor, error)
	// Read returns the next non-empty batch from c, waiting as needed,
	// bounded by ctx. ErrOutOfRange: the feed cannot continue c.
	Read(ctx context.Context, c Cursor, o ReadOpts) (Batch, error)
	// Want registers a tap's ReadOpts until release; the feed fetches
	// receipts when a block is pinned and any registered MayMatch wants it.
	Want(o ReadOpts) (release func())
	// LogsHead is the highest pin up to which every block's receipts are
	// held or not needed; ws logs taps measure lag from it.
	LogsHead() Cursor
}

var ErrOutOfRange = errors.New("cursor out of range")

// Why a tap's cursor went out of range; erpc_tap_out_of_range_total labels.
const (
	OutOfRangeBelowWindow    = "below_window"
	OutOfRangeForkLeftWindow = "fork_left_window"
	OutOfRangeCursorUnknown  = "cursor_unknown"
	// OutOfRangeLag: a transport's wait bound ended the tap (spec §9.2).
	OutOfRangeLag = "lag"
)

// OutOfRangeError is ErrOutOfRange with its reason.
type OutOfRangeError struct{ Reason string }

func (e OutOfRangeError) Error() string        { return ErrOutOfRange.Error() + ": " + e.Reason }
func (e OutOfRangeError) Is(target error) bool { return target == ErrOutOfRange }

// Topic is what a source feeds: newHeads, or
// newPendingTransactions:<variant>.
type Topic string

const TopicNewHeads Topic = SubTypeNewHeads

// Intake is what sources report into; erpc implements it per network.
type Intake interface {
	Header(upstreamId string, raw json.RawMessage)
	// Subscribed reports a push subscribe's answer for t: nil once it is
	// live, else the upstream's error.
	Subscribed(upstreamId string, t Topic, err error)
}

// Source feeds one upstream's observations over one transport.
type Source interface {
	Set(topics []Topic) // what to feed now; idempotent; empty stops
}
