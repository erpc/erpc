package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

// MempoolWakeEvery is the shortest time between two reads of a mempool
// tap, so a busy mempool reaches it in batches; a var so tests can change it.
var MempoolWakeEvery = util.NewDuration(50 * time.Millisecond)

// Taps runs read loops: read, emit, repeat. It counts open taps per network
// and topic, so sources know what to feed.
type Taps struct {
	projectId      string
	onDemandChange func(networkId string)

	mu   sync.Mutex
	open map[string]map[Topic]int // networkId -> topic -> open taps
}

// NewTaps returns an empty Taps of one project; projectId labels metrics.
// onDemandChange, if set, is called whenever a network's demand gains or
// loses a topic; it must never block.
func NewTaps(projectId string, onDemandChange func(networkId string)) *Taps {
	return &Taps{projectId: projectId, onDemandChange: onDemandChange, open: make(map[string]map[Topic]int)}
}

// RunChain runs one tap on f until ctx ends, emit fails, or f returns an
// error. start nil starts at f.Head. transport and kind label metrics.
// emit returns how many of the batch's Applied blocks it delivered: a tap
// that delivered fewer, as when a block left the pins before its write,
// continues after the last one it delivered.
func (t *Taps) RunChain(ctx context.Context, networkId, transport, kind string,
	f ChainFeed, start *Cursor, o ReadOpts, emit func(Batch) (delivered int, err error)) (err error) {
	t.add(networkId, TopicNewHeads)
	defer t.remove(networkId, TopicNewHeads)
	open := telemetry.GaugeHandle(telemetry.MetricTaps, t.projectId, networkId, transport, kind)
	open.Inc()
	defer open.Dec()
	defer func() {
		var oor OutOfRangeError
		if errors.As(err, &oor) {
			telemetry.CounterHandle(telemetry.MetricTapOutOfRangeTotal,
				t.projectId, networkId, transport, kind, oor.Reason).Inc()
		}
	}()
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"tap",
				fmt.Sprintf("network:%s transport:%s kind:%s", networkId, transport, kind),
				common.ErrorFingerprint(rec),
			).Inc()
			err = fmt.Errorf("tap panicked: %v", rec)
		}
	}()
	if o.MayMatch != nil || o.Resumable {
		release := f.Want(o)
		defer release()
	}

	var c Cursor
	if start == nil {
		if c, err = f.Head(ctx); err != nil {
			return err
		}
	} else {
		c = *start
	}
	for {
		b, err := f.Read(ctx, c, o)
		if err != nil {
			return err
		}
		if !linked(c, b) {
			t.gap(networkId, transport, kind, c, b)
		}
		n, err := emit(b)
		if err != nil {
			return err
		}
		if n < len(b.Applied) {
			blk := b.Applied[n]
			c = Cursor{Number: blk.Number - min(blk.Number, 1), Hash: blk.ParentHash}
		} else {
			c = b.Next
		}
	}
}

// linked reports whether b continues a tap at c (spec §15.3): its Applied
// blocks extend c, or its Reverted blocks run from c down to a fork point
// its Applied blocks extend.
func linked(c Cursor, b Batch) bool {
	at := c
	if len(b.Reverted) > 0 {
		if b.Reverted[0].Number != c.Number || b.Reverted[0].Hash != c.Hash {
			return false
		}
		for i := 1; i < len(b.Reverted); i++ {
			if up := b.Reverted[i-1]; up.ParentHash != b.Reverted[i].Hash || up.Number != b.Reverted[i].Number+1 {
				return false
			}
		}
		fork := b.Reverted[len(b.Reverted)-1]
		at = Cursor{Number: fork.Number - min(fork.Number, 1), Hash: fork.ParentHash}
	}
	for _, blk := range b.Applied {
		if blk.ParentHash != at.Hash || blk.Number != at.Number+1 {
			return false
		}
		at = Cursor{Number: blk.Number, Hash: blk.Hash}
	}
	return true
}

// gap counts and logs a batch that does not continue the tap's cursor c.
func (t *Taps) gap(networkId, transport, kind string, c Cursor, b Batch) {
	ev := common.ViolateInvariant(&log.Logger, t.projectId, networkId, common.InvariantTapGap).
		Str("tapTransport", transport).Str("tapKind", kind).
		Uint64("cursor", c.Number).Hex("cursorHash", c.Hash[:]).
		Int("reverted", len(b.Reverted)).Int("applied", len(b.Applied))
	if len(b.Reverted) > 0 {
		r := b.Reverted[len(b.Reverted)-1]
		ev = ev.Uint64("revertedFirst", b.Reverted[0].Number).Hex("revertedFirstHash", b.Reverted[0].Hash[:]).
			Uint64("revertedLast", r.Number).Hex("revertedLastParentHash", r.ParentHash[:])
	}
	if len(b.Applied) > 0 {
		a := b.Applied[0]
		ev = ev.Uint64("appliedFirst", a.Number).Hex("appliedFirstHash", a.Hash[:]).Hex("appliedFirstParentHash", a.ParentHash[:])
	}
	ev.Msg("tap batch does not link to its cursor")
}

// RunMempool runs one tap on f's variant from after its Tail until ctx
// ends, emit fails, or f returns an error other than ErrOutOfRange: out of
// range skips to the newest. transport labels metrics.
func (t *Taps) RunMempool(ctx context.Context, networkId, transport, variant string,
	f MempoolFeed, emit func([]json.RawMessage) error) (err error) {
	kind := SubTypeNewPendingTransactions
	topic := PendingTxTopic(variant)
	t.add(networkId, topic)
	defer t.remove(networkId, topic)
	open := telemetry.GaugeHandle(telemetry.MetricTaps, t.projectId, networkId, transport, kind)
	open.Inc()
	defer open.Dec()
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"tap",
				fmt.Sprintf("network:%s transport:%s kind:%s", networkId, transport, kind),
				common.ErrorFingerprint(rec),
			).Inc()
			err = fmt.Errorf("tap panicked: %v", rec)
		}
	}()

	seq := f.Tail(variant)
	var last time.Time
	for {
		if wait := MempoolWakeEvery.Load() - time.Since(last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		txs, next, err := f.Read(ctx, variant, seq)
		if errors.Is(err, ErrOutOfRange) {
			telemetry.CounterHandle(telemetry.MetricTapOutOfRangeTotal,
				t.projectId, networkId, transport, kind, OutOfRangeLag).Inc()
			seq = f.Tail(variant)
			continue
		}
		if err != nil {
			return err
		}
		last = time.Now()
		if err := emit(txs); err != nil {
			return err
		}
		seq = next
	}
}

// Demand returns the topics the open taps of networkId need.
func (t *Taps) Demand(networkId string) []Topic {
	t.mu.Lock()
	defer t.mu.Unlock()
	topics := make([]Topic, 0, len(t.open[networkId]))
	for topic := range t.open[networkId] {
		topics = append(topics, topic)
	}
	slices.Sort(topics)
	return topics
}

func (t *Taps) add(networkId string, topic Topic) {
	t.mu.Lock()
	byTopic := t.open[networkId]
	if byTopic == nil {
		byTopic = make(map[Topic]int)
		t.open[networkId] = byTopic
	}
	byTopic[topic]++
	changed := byTopic[topic] == 1
	t.mu.Unlock()
	if changed && t.onDemandChange != nil {
		t.onDemandChange(networkId)
	}
}

func (t *Taps) remove(networkId string, topic Topic) {
	t.mu.Lock()
	byTopic := t.open[networkId]
	byTopic[topic]--
	changed := byTopic[topic] == 0
	if changed {
		delete(byTopic, topic)
		if len(byTopic) == 0 {
			delete(t.open, networkId)
		}
	}
	t.mu.Unlock()
	if changed && t.onDemandChange != nil {
		t.onDemandChange(networkId)
	}
}
