// Package indexer is the chain- and transport-neutral core between sources
// and transports: the chain and relay feed contracts, intake, sources and
// taps. It must not import erpc, architecture/evm, clients, or any
// transport library.
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
	// Pace is how often the tap needs a newer head: 0 every block. The
	// tap's demand carries it (Taps.Pace); a Read ignores it.
	Pace time.Duration
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
)

// Why a tap ended; the reason label of erpc_tap_end_total.
const (
	TapEndOutOfRange = "out_of_range"
	TapEndLagging    = "lagging"
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

// RelayFeed is the reader side of one network's relayed messages.
type RelayFeed interface {
	Tail(t Topic) uint64 // newest sequence; a new tap starts after it
	// Read waits for messages after seq, bounded by ctx.
	// ErrOutOfRange: seq left the ring.
	Read(ctx context.Context, t Topic, after uint64) (msgs []json.RawMessage, next uint64, err error)
}

// Topic is what a source feeds: newHeads, or a relay topic
// <kind>:<variant>.
type Topic string

// TopicNewHeads is the chain's heads; a topic of the sources, named as the
// subscription kind that wants them.
const TopicNewHeads Topic = "newHeads"

// RelayTopic is the topic of one variant of a relayed kind.
func RelayTopic(kind, variant string) Topic { return Topic(kind + ":" + variant) }

// Relay is t's kind and variant, if t is a relay topic.
func (t Topic) Relay() (kind, variant string, ok bool) {
	return strings.Cut(string(t), ":")
}

// RelayVariant names the variant of a relayed kind's subscribe params,
// those after the kind: JSON with sorted keys, so equal params from any
// client are one variant.
func RelayVariant(params []interface{}) (string, error) {
	if len(params) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RelayParams are the subscribe params variant names, after the kind.
func RelayParams(variant string) ([]interface{}, error) {
	var params []interface{}
	if err := json.Unmarshal([]byte(variant), &params); err != nil {
		return nil, err
	}
	return params, nil
}

// Intake is what sources report into; erpc implements it per network.
type Intake interface {
	Header(upstreamId string, raw json.RawMessage)
	Relayed(upstreamId string, t Topic, raw json.RawMessage)
	// Subscribed reports a push subscribe's answer for t: nil once it is
	// live, else the upstream's error.
	Subscribed(upstreamId string, t Topic, err error)
}

// Source feeds one upstream's observations over one transport.
type Source interface {
	Set(topics []Topic) // what to feed now; idempotent; empty stops
}
