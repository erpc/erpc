package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// tapSends checks each block one tap writes against the transport
// invariants (spec §15.3) and times its delivery (§17). lane is the tap's
// lane, nil for a selector that forms none.
type tapSends struct {
	nw              *Network
	selector        string
	lane            *servedTipLane
	transport, kind string
}

func newTapSends(nw *Network, selector, transport, kind string) tapSends {
	return tapSends{nw: nw, selector: selector, lane: nw.laneFor(selector), transport: transport, kind: kind}
}

// sendable reports whether blk is at or below the tracker's confirmed head
// now. A block a Read returned can lose its confirmation before its write,
// as when hash voters arrive and raise the votes a pin needs; the tap ends
// its batch there, and its next Read waits for the confirmed head. (The
// tracker caps the confirmed head at the lane's Latest, so a block above
// Latest is caught there.)
func (s tapSends) sendable(blk indexer.Block) bool {
	conf, ok := s.nw.chainTracker.Confirmed()
	return ok && blk.Number <= conf.Number
}

// write writes blk with fn. A block above the lane's Latest or above the
// tracker's confirmed head at write time is a violation; a written one
// raises the lane's highest sent block (latest_regressed).
func (s tapSends) write(blk indexer.Block, fn func() error) error {
	s.check(blk)
	if err := fn(); err != nil {
		return err
	}
	if !blk.Ready.IsZero() {
		telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, s.transport, s.kind).Observe(time.Since(blk.Ready).Seconds())
	}
	if s.lane != nil {
		s.lane.noteSent(blk.Number)
	}
	return nil
}

func (s tapSends) check(blk indexer.Block) {
	if latest := s.nw.laneLatest(s.lane, s.selector); blk.Number > latest {
		s.violate(common.InvariantSentAboveLatest, blk).Uint64("latest", latest).
			Msg("tap writes a block above its lane's Latest")
	}
	if conf, ok := s.nw.chainTracker.Confirmed(); !ok || blk.Number > conf.Number {
		s.violate(common.InvariantSentUnconfirmed, blk).Uint64("confirmed", conf.Number).Hex("confirmedHash", conf.Hash[:]).
			Msg("tap writes a block above the chain tracker's confirmed head")
	}
}

func (s tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// tapSends checks each block one tap writes against the transport
// invariants (spec §15.3) and times its delivery (§17). lane is the tap's
// lane, nil for a selector that forms none.
type tapSends struct {
	nw              *Network
	selector        string
	lane            *servedTipLane
	transport, kind string
}

func newTapSends(nw *Network, selector, transport, kind string) tapSends {
	return tapSends{nw: nw, selector: selector, lane: nw.laneFor(selector), transport: transport, kind: kind}
}

// sendable reports whether blk is at or below the tracker's confirmed head
// now. A block a Read returned can lose its confirmation before its write,
// as when hash voters arrive and raise the votes a pin needs; the tap ends
// its batch there, and its next Read waits for the confirmed head. (The
// tracker caps the confirmed head at the lane's Latest, so a block above
// Latest is caught there.)
func (s tapSends) sendable(blk indexer.Block) bool {
	conf, ok := s.nw.chainTracker.Confirmed()
	return ok && blk.Number <= conf.Number
}

// write writes blk with fn. A block above the lane's Latest or above the
// tracker's confirmed head at write time is a violation; a written one
// raises the lane's highest sent block (latest_regressed).
func (s tapSends) write(blk indexer.Block, fn func() error) error {
	s.check(blk)
	if err := fn(); err != nil {
		return err
	}
	if !blk.Ready.IsZero() {
		telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, s.transport, s.kind).Observe(time.Since(blk.Ready).Seconds())
	}
	if s.lane != nil {
		s.lane.noteSent(blk.Number)
	}
	return nil
}

func (s tapSends) check(blk indexer.Block) {
	if latest := s.nw.laneLatest(s.lane, s.selector); blk.Number > latest {
		s.violate(common.InvariantSentAboveLatest, blk).Uint64("latest", latest).
			Msg("tap writes a block above its lane's Latest")
	}
	if conf, ok := s.nw.chainTracker.Confirmed(); !ok || blk.Number > conf.Number {
		s.violate(common.InvariantSentUnconfirmed, blk).Uint64("confirmed", conf.Number).Hex("confirmedHash", conf.Hash[:]).
			Msg("tap writes a block above the chain tracker's confirmed head")
	}
}

func (s tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// tapSends checks each block one tap writes against the transport
// invariants (spec §15.3) and times its delivery (§17). lane is the tap's
// lane, nil for a selector that forms none.
type tapSends struct {
	nw              *Network
	selector        string
	lane            *servedTipLane
	transport, kind string
}

func newTapSends(nw *Network, selector, transport, kind string) tapSends {
	return tapSends{nw: nw, selector: selector, lane: nw.laneFor(selector), transport: transport, kind: kind}
}

// sendable reports whether blk is at or below the tracker's confirmed head
// now. A block a Read returned can lose its confirmation before its write,
// as when hash voters arrive and raise the votes a pin needs; the tap ends
// its batch there, and its next Read waits for the confirmed head. (The
// tracker caps the confirmed head at the lane's Latest, so a block above
// Latest is caught there.)
func (s tapSends) sendable(blk indexer.Block) bool {
	conf, ok := s.nw.chainTracker.Confirmed()
	return ok && blk.Number <= conf.Number
}

// write writes blk with fn. A block above the lane's Latest or above the
// tracker's confirmed head at write time is a violation; a written one
// raises the lane's highest sent block (latest_regressed).
func (s tapSends) write(blk indexer.Block, fn func() error) error {
	s.check(blk)
	if err := fn(); err != nil {
		return err
	}
	if !blk.Ready.IsZero() {
		telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, s.transport, s.kind).Observe(time.Since(blk.Ready).Seconds())
	}
	if s.lane != nil {
		s.lane.noteSent(blk.Number)
	}
	return nil
}

func (s tapSends) check(blk indexer.Block) {
	if latest := s.nw.laneLatest(s.lane, s.selector); blk.Number > latest {
		s.violate(common.InvariantSentAboveLatest, blk).Uint64("latest", latest).
			Msg("tap writes a block above its lane's Latest")
	}
	if conf, ok := s.nw.chainTracker.Confirmed(); !ok || blk.Number > conf.Number {
		s.violate(common.InvariantSentUnconfirmed, blk).Uint64("confirmed", conf.Number).Hex("confirmedHash", conf.Hash[:]).
			Msg("tap writes a block above the chain tracker's confirmed head")
	}
}

func (s tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// tapSends checks each block one tap writes against the transport
// invariants (spec §15.3) and times its delivery (§17). lane is the tap's
// lane, nil for a selector that forms none.
type tapSends struct {
	nw              *Network
	selector        string
	lane            *servedTipLane
	transport, kind string
}

func newTapSends(nw *Network, selector, transport, kind string) tapSends {
	return tapSends{nw: nw, selector: selector, lane: nw.laneFor(selector), transport: transport, kind: kind}
}

// sendable reports whether blk is at or below the tracker's confirmed head
// now. A block a Read returned can lose its confirmation before its write,
// as when hash voters arrive and raise the votes a pin needs; the tap ends
// its batch there, and its next Read waits for the confirmed head. (The
// tracker caps the confirmed head at the lane's Latest, so a block above
// Latest is caught there.)
func (s tapSends) sendable(blk indexer.Block) bool {
	conf, ok := s.nw.chainTracker.Confirmed()
	return ok && blk.Number <= conf.Number
}

// write writes blk with fn. A block above the lane's Latest or above the
// tracker's confirmed head at write time is a violation; a written one
// raises the lane's highest sent block (latest_regressed).
func (s tapSends) write(blk indexer.Block, fn func() error) error {
	s.check(blk)
	if err := fn(); err != nil {
		return err
	}
	if !blk.Ready.IsZero() {
		telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, s.transport, s.kind).Observe(time.Since(blk.Ready).Seconds())
	}
	if s.lane != nil {
		s.lane.noteSent(blk.Number)
	}
	return nil
}

func (s tapSends) check(blk indexer.Block) {
	if latest := s.nw.laneLatest(s.lane, s.selector); blk.Number > latest {
		s.violate(common.InvariantSentAboveLatest, blk).Uint64("latest", latest).
			Msg("tap writes a block above its lane's Latest")
	}
	if conf, ok := s.nw.chainTracker.Confirmed(); !ok || blk.Number > conf.Number {
		s.violate(common.InvariantSentUnconfirmed, blk).Uint64("confirmed", conf.Number).Hex("confirmedHash", conf.Hash[:]).
			Msg("tap writes a block above the chain tracker's confirmed head")
	}
}

func (s tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
