package indexer

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// Taps runs read loops: read, emit, repeat. It counts open taps per network
// and topic, so sources know what to feed.
type Taps struct {
	onDemandChange func(networkId string)

	mu   sync.Mutex
	open map[string]map[Topic]int // networkId -> topic -> open taps
}

// NewTaps returns an empty Taps. onDemandChange, if set, is called whenever a
// network's demand gains or loses a topic; it must never block.
func NewTaps(onDemandChange func(networkId string)) *Taps {
	return &Taps{onDemandChange: onDemandChange, open: make(map[string]map[Topic]int)}
}

// RunChain runs one tap on f until ctx ends, emit fails, or f returns an
// error. start nil starts at f.Head. transport and kind label metrics.
func (t *Taps) RunChain(ctx context.Context, networkId, transport, kind string,
	f ChainFeed, start *Cursor, o ReadOpts, emit func(Batch) error) (err error) {
	t.add(networkId, TopicNewHeads)
	defer t.remove(networkId, TopicNewHeads)
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
	if o.MayMatch != nil {
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
		if err := emit(b); err != nil {
			return err
		}
		c = b.Next
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
