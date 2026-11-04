package erpc

import (
	"context"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// tapSends checks each block one tap writes against the transport
// invariants (spec §15.3) and times its delivery (§17). selector is the
// tap's use-upstream selector, "" for none.
type tapSends struct {
	nw              *Network
	selector        string
	transport, kind string
	delivery        prometheus.Observer
}

func newTapSends(nw *Network, selector, transport, kind string) *tapSends {
	return &tapSends{nw: nw, selector: selector, transport: transport, kind: kind,
		delivery: telemetry.ObserverHandle(telemetry.MetricTapDeliverySeconds, transport, kind)}
}

// sendSnapshot is one decision to write a block: the network-wide
// tracker's state, read once, and for a lane tap its lane's Head, read
// once (headFor), with the channels closed once either may have moved.
type sendSnapshot struct {
	view        evm.View
	head        indexer.Cursor // the Head the block may not pass
	headOK      bool
	laneChanged <-chan struct{} // nil without a lane
}

// gate reads blk's snapshot and reports whether blk may be written: pinned
// in the state's pins, at or below its Head, and for a lane tap at or below
// the lane's Head, whose hash the pins hold there (spec §6.5 one view). A
// block a Read returned can leave the view before its write, as when a
// reorg moves the pins; the tap ends its batch there (hold), and its next
// Read waits.
func (s *tapSends) gate(blk indexer.Block) (sendSnapshot, bool) {
	snap := sendSnapshot{view: s.nw.chainTracker.View()}
	snap.head, snap.headOK = snap.view.Head()
	if s.selector != "" && snap.headOK {
		if lt := s.nw.laneTracker(s.selector); lt != nil {
			snap.laneChanged = lt.Changed()
		}
		lh := s.nw.headFor(tipScope{sel: s.selector})
		switch {
		case lh.Latest <= 0:
			snap.headOK = false
		case uint64(lh.Latest) < snap.head.Number:
			pin, ok := snap.view.PinAt(uint64(lh.Latest))
			snap.head, snap.headOK = pin, ok && pin.Hash == lh.LatestHash
		}
	}
	c := indexer.Cursor{Number: blk.Number, Hash: blk.Hash}
	return snap, snap.view.Pinned(c) && snap.headOK && blk.Number <= snap.head.Number
}

// hold is the end of a batch at blk, which gate held. A pinned block held
// only by a lane's Head would come back from the tap's next Read at once:
// hold waits for the network's or the lane's state to move first, or ctx
// to end. Other holds return at once; the next Read waits.
func (s *tapSends) hold(ctx context.Context, blk indexer.Block, snap sendSnapshot) error {
	if snap.laneChanged == nil || !snap.view.Pinned(indexer.Cursor{Number: blk.Number, Hash: blk.Hash}) {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-snap.view.Changed():
	case <-snap.laneChanged:
	}
	return nil
}

// tapDelivery times one block's delivery (§17): its transport calls done
// once the write of the block's last message returned.
type tapDelivery struct {
	obs   prometheus.Observer
	ready time.Time
}

func (d tapDelivery) done() {
	if d.obs != nil && !d.ready.IsZero() {
		d.obs.Observe(time.Since(d.ready).Seconds())
	}
}

// write writes blk, gated on snap, with fn. A block above the snapshot's
// Head is a violation.
func (s *tapSends) write(blk indexer.Block, snap sendSnapshot, fn func(tapDelivery) error) error {
	if !snap.headOK || blk.Number > snap.head.Number {
		s.violate(common.InvariantSentAboveHead, blk).Uint64("head", snap.head.Number).Hex("headHash", snap.head.Hash[:]).
			Msg("tap writes a block above its lane's Head")
	}
	return fn(tapDelivery{obs: s.delivery, ready: blk.Ready})
}

func (s *tapSends) violate(inv common.Invariant, blk indexer.Block) *zerolog.Event {
	return common.ViolateInvariant(s.nw.logger, s.nw.projectId, s.nw.Label(), inv).
		Str("tapTransport", s.transport).Str("tapKind", s.kind).Str("lane", s.selector).
		Uint64("block", blk.Number).Hex("blockHash", blk.Hash[:]).Hex("parentHash", blk.ParentHash[:])
}
