package erpc

import (
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
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
	delivery        prometheus.Observer
}

func newTapSends(nw *Network, selector, transport, kind string) *tapSends {
	return &tapSends{nw: nw, selector: selector, lane: nw.laneFor(selector), transport: transport, kind: kind,
		delivery: telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, transport, kind)}
}

// sendSnapshot is one decision to write a block: the tracker's state and
// the lane's Latest, each read once.
type sendSnapshot struct {
	pinned      bool
	confirmed   indexer.Cursor
	confirmedOK bool
	latest      uint64
}

// gate reads blk's snapshot and reports whether blk may be written: pinned,
// at or below the tracker's confirmed head and at or below the lane's
// Latest. A block a Read returned can leave any of them before its write,
// as when hash voters arrive and raise the votes a pin needs, or a lane's
// Latest falls before the tracker caps its confirmed head; the tap ends its
// batch there, and its next Read waits.
func (s *tapSends) gate(blk indexer.Block) (sendSnapshot, bool) {
	var snap sendSnapshot
	snap.pinned, snap.confirmed, snap.confirmedOK = s.nw.chainTracker.SendState(indexer.Cursor{Number: blk.Number, Hash: blk.Hash})
	snap.latest = s.nw.laneLatest(s.lane, s.selector)
	return snap, snap.pinned && snap.confirmedOK && blk.Number <= snap.confirmed.Number && blk.Number <= snap.latest
}

// write writes blk, gated on snap, with fn. A block above the snapshot's
// Latest or confirmed head is a violation; a written one raises the lane's
// highest sent block (latest_regressed).
func (s *tapSends) write(blk indexer.Block, snap sendSnapshot, fn func() error) error {
	s.check(blk, snap)
	if err := fn(); err != nil {
		return err
	}
	if !blk.Ready.IsZero() {
		s.delivery.Observe(time.Since(blk.Ready).Seconds())
	}
	if s.lane != nil {
		s.lane.noteSent(blk.Number)
	}
	return nil
}

func (s *tapSends) check(blk indexer.Block, snap sendSnapshot) {
	if blk.Number > snap.latest {
		s.violate(common.InvariantSentAboveLatest, blk).Uint64("latest", snap.latest).
			Msg("tap writes a block above its lane's Latest")
	}
	if !snap.confirmedOK || blk.Number > snap.confirmed.Number {
		s.violate(common.InvariantSentUnconfirmed, blk).Uint64("confirmed", snap.confirmed.Number).Hex("confirmedHash", snap.confirmed.Hash[:]).
			Msg("tap writes a block above the chain tracker's confirmed head")
	}
}

func (s *tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
