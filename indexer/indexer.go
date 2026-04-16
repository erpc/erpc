package indexer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Options configures Indexer construction. Defaults (zero values) are
// sensible for all fields; override only for tuning.
type Options struct {
	// DedupWindowSize is the per-filter seen-set capacity. 0 = default.
	DedupWindowSize int
	// CanonicalChainDepth is the ring-buffer size of the per-network
	// canonical-chain tracker. 0 = default. Controls how deep a reorg
	// we can fully resolve.
	CanonicalChainDepth int
	// Now is the clock used for event timestamps. Tests inject a fake
	// clock; nil falls back to time.Now.
	Now func() time.Time
}

// Indexer is the transport-neutral core: ingresses push StreamEvents via
// Sink.Ingest; the indexer dedupes, sequences, and lifecycle-tags them,
// then fans out to every interested egress.
//
// The Indexer itself implements Sink so adapters can call indexer.Ingest
// directly — the choice is deliberate: synchronous Ingest keeps
// "update-before-dedup" ordering guarantees from the ingress goroutine
// without goroutine-per-event channel gymnastics.
type Indexer struct {
	logger *zerolog.Logger
	opts   Options

	networks sync.Map // networkId -> *networkState
	egresses sync.Map // egress Name() -> EventEgress
	seq      atomic.Uint64
}

// networkState holds the indexer's per-network bookkeeping: the
// headMarker packages the most-recent head seen for a network so
// networkState can store (num, hash) atomically. Treated as immutable
// once published via lastHead.Store / CompareAndSwap; readers load
// and may see nil before the first head arrives.
type headMarker struct {
	num  int64
	hash string
}

// NetworkHandle for finality lookups, the ingresses feeding it, and the
// dedup windows for each active filter (plus the newHeads window).
type networkState struct {
	handle NetworkHandle

	ingressMu sync.RWMutex
	ingresses map[string]EventIngress // Name() -> ingress

	// newHeads dedup: most-recently-delivered (blockNumber, blockHash)
	// packed into a single pointer so the check+advance is a single
	// atomic CAS — separate atomics on num and hash would leave readers
	// able to see num updated before hash, and producers able to both
	// pass the "load, check, store" sequence with the same stale num.
	// We saw the latter in prod against evm:1101 where four upstream WS
	// sources delivered the same head within ~1ms and both raced past
	// the dedup. Fallback DedupWindow still catches the rare
	// "older-but-not-newest" case (out-of-order delivery on a reorg
	// boundary).
	lastHead     atomic.Pointer[headMarker]
	headFallback *DedupWindow

	// Per-filter dedup windows: filterHash -> *DedupWindow.
	filterMu     sync.RWMutex
	filterDedup  map[string]*DedupWindow
	filterRefcnt map[string]int // clients × filterHash; triggers RemoveFilter at 0

	// chain records the last N canonical heads for this network plus
	// an index of delivered logs keyed by blockHash. Drives reorg
	// detection and Removed=true re-emission.
	chain *canonicalChain
}

// New returns an empty Indexer. Networks must be registered via
// RegisterNetwork before any ingress can push events.
func New(logger *zerolog.Logger, opts Options) *Indexer {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DedupWindowSize <= 0 {
		opts.DedupWindowSize = DefaultDedupWindowSize
	}
	return &Indexer{
		logger: logger,
		opts:   opts,
	}
}

// RegisterNetwork installs a NetworkHandle for the indexer to consult when
// tagging lifecycle and routing per-source state-poller updates. Safe to
// call more than once for the same network (idempotent on handle identity).
func (i *Indexer) RegisterNetwork(nw NetworkHandle) *networkState {
	if ns, ok := i.networks.Load(nw.Id()); ok {
		return ns.(*networkState)
	}
	ns := &networkState{
		handle:       nw,
		ingresses:    make(map[string]EventIngress),
		headFallback: NewDedupWindow(i.opts.DedupWindowSize),
		filterDedup:  make(map[string]*DedupWindow),
		filterRefcnt: make(map[string]int),
		chain:        newCanonicalChain(i.opts.CanonicalChainDepth),
	}
	actual, _ := i.networks.LoadOrStore(nw.Id(), ns)
	return actual.(*networkState)
}

// AddIngress starts an ingress and hands it the NetworkHandle registered
// for networkId. The ingress pushes events at the indexer (which
// implements Sink). Returns an error if the network has not been
// registered yet.
func (i *Indexer) AddIngress(ctx context.Context, networkId string, ing EventIngress) error {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return errNetworkNotRegistered(networkId)
	}
	ns := nsRaw.(*networkState)
	ns.ingressMu.Lock()
	ns.ingresses[ing.Name()] = ing
	ns.ingressMu.Unlock()
	return ing.Start(ctx, ns.handle, i)
}

// Attach registers an egress. The returned detach function removes the
// egress — callers that care about cleanup (client-connection closes)
// must invoke it.
func (i *Indexer) Attach(eg EventEgress) (detach func()) {
	i.egresses.Store(eg.Name(), eg)
	return func() { i.egresses.Delete(eg.Name()) }
}

// EnsureFilter fans a filter subscription out to every ingress registered
// on the given network, and tracks a per-filter refcount so
// ReleaseFilter can decide when to tear it down upstream.
func (i *Indexer) EnsureFilter(ctx context.Context, networkId, subType string, params []interface{}) (paramsHash string, err error) {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return "", errNetworkNotRegistered(networkId)
	}
	ns := nsRaw.(*networkState)
	paramsHash = BuildParamsKey(params)

	ns.filterMu.Lock()
	if _, ok := ns.filterDedup[paramsHash]; !ok {
		ns.filterDedup[paramsHash] = NewDedupWindow(i.opts.DedupWindowSize)
	}
	ns.filterRefcnt[paramsHash]++
	first := ns.filterRefcnt[paramsHash] == 1
	ns.filterMu.Unlock()

	if !first {
		return paramsHash, nil
	}

	// First subscriber — fan out to all ingresses. We snapshot the
	// ingress set under rlock then call EnsureFilter without the lock
	// held; per-ingress calls can be slow (RPC round-trip on WS).
	ns.ingressMu.RLock()
	ings := make([]EventIngress, 0, len(ns.ingresses))
	for _, ing := range ns.ingresses {
		ings = append(ings, ing)
	}
	ns.ingressMu.RUnlock()

	var firstErr error
	for _, ing := range ings {
		if err := ing.EnsureFilter(ctx, subType, paramsHash, params); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			i.logger.Warn().Err(err).Str("ingress", ing.Name()).Str("networkId", networkId).
				Str("subType", subType).Str("paramsHash", paramsHash).
				Msg("ingress EnsureFilter failed")
		}
	}
	return paramsHash, firstErr
}

// ReleaseFilter decrements the refcount on the filter and tears the
// subscription down on every ingress when it hits zero. Callers supply
// paramsHash (returned by EnsureFilter) rather than params.
func (i *Indexer) ReleaseFilter(ctx context.Context, networkId, subType, paramsHash string) {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return
	}
	ns := nsRaw.(*networkState)

	ns.filterMu.Lock()
	ns.filterRefcnt[paramsHash]--
	remove := ns.filterRefcnt[paramsHash] <= 0
	if remove {
		delete(ns.filterRefcnt, paramsHash)
		delete(ns.filterDedup, paramsHash)
	}
	ns.filterMu.Unlock()

	if !remove {
		return
	}
	ns.ingressMu.RLock()
	ings := make([]EventIngress, 0, len(ns.ingresses))
	for _, ing := range ns.ingresses {
		ings = append(ings, ing)
	}
	ns.ingressMu.RUnlock()

	for _, ing := range ings {
		if err := ing.RemoveFilter(ctx, subType, paramsHash); err != nil {
			i.logger.Warn().Err(err).Str("ingress", ing.Name()).Str("networkId", networkId).
				Str("subType", subType).Str("paramsHash", paramsHash).
				Msg("ingress RemoveFilter failed")
		}
	}
}

// Ingest is the hot-path entry point for ingress adapters. It updates
// per-source state (via NetworkHandle.SuggestLatestBlock for headed
// events), dedupes, lifecycle-tags, emits reorg invalidations, and
// fans out to every interested egress.
func (i *Indexer) Ingest(ev StreamEvent) {
	nsRaw, ok := i.networks.Load(ev.NetworkId)
	if !ok {
		return
	}
	ns := nsRaw.(*networkState)

	// State-poller update before dedup. Every observation feeds the
	// per-upstream latest-block tracker, even if the head is a dup at
	// the indexer level — otherwise a lagging source's state poller
	// stalls on the first dup.
	if ev.Kind == KindNewHead && !ev.Block.Zero() && ev.SourceId != "" {
		ns.handle.SuggestLatestBlock(ev.SourceId, ev.Block.Number)
	}

	// Dedup.
	if !i.dedupe(ns, &ev) {
		return
	}

	// Detect and emit reorg invalidations BEFORE delivering the new
	// head. Consumers see: (removed logs) → reorg summary → new head.
	if ev.Kind == KindNewHead && !ev.Block.Zero() {
		if evicted := ns.chain.observeHead(ev.Block); len(evicted) > 0 {
			i.emitReorgInvalidations(ns, evicted)
		}
	}

	// Index logs so a later reorg can re-emit them with Removed=true.
	if ev.Kind == KindLog {
		// Best-effort: if the payload has a parseable blockHash, record
		// the log against it. Logs without a blockHash can't be
		// invalidated — safe to drop from the index.
		if blockRef := parseLogBlockRef(ev.Payload); blockRef.Hash != "" {
			ns.chain.indexLog(loggedLog{
				filterHash: ev.FilterHash,
				networkID:  ev.NetworkId,
				sourceID:   ev.SourceId,
				block:      blockRef,
				payload:    ev.Payload,
			})
		}
	}

	// Build the indexed event.
	out := IndexedEvent{
		StreamEvent: ev,
		Seq:         i.seq.Add(1),
		Lifecycle:   i.classify(ns, ev),
	}
	// Upstream-asserted removed flag. Chain-tracker-driven removal goes
	// through emitReorgInvalidations; this handles the case where the
	// upstream itself has already classified the log as reorged-out.
	if ev.Kind == KindLog {
		out.Removed = logRemoved(ev.Payload)
	}

	i.fanOut(out)
}

// emitReorgInvalidations fans out a KindReorg summary followed by
// Removed=true copies of every log indexed against the evicted blocks.
// Called under ingest's caller goroutine; Deliver is non-blocking by
// contract so this stays cheap even under deep reorgs.
func (i *Indexer) emitReorgInvalidations(ns *networkState, evicted []BlockRef) {
	if len(evicted) == 0 {
		return
	}
	networkID := ns.handle.Id()

	// One KindReorg summary carrying the evicted BlockRefs.
	reorgPayload, err := reorgSummaryPayload(evicted)
	if err == nil {
		i.fanOut(IndexedEvent{
			StreamEvent: StreamEvent{
				Kind:      KindReorg,
				NetworkId: networkID,
				Payload:   reorgPayload,
			},
			Seq:       i.seq.Add(1),
			Lifecycle: LifeSoft,
		})
	}

	// One Removed=true emission per indexed log in each evicted block.
	for _, blk := range evicted {
		logs := ns.chain.drainLogsFor(blk.Hash)
		for _, lg := range logs {
			i.fanOut(IndexedEvent{
				StreamEvent: StreamEvent{
					Kind:       KindLog,
					NetworkId:  lg.networkID,
					SourceId:   lg.sourceID,
					FilterHash: lg.filterHash,
					Block:      lg.block,
					Payload:    lg.payload,
				},
				Seq:       i.seq.Add(1),
				Lifecycle: LifeSoft,
				Removed:   true,
			})
		}
	}
}

// dedupe returns true if the event should be delivered, false if it's a
// dupe. For newHeads we use an optimistic fast-path on (last number,
// last hash); misses fall through to a bounded DedupWindow.
func (i *Indexer) dedupe(ns *networkState, ev *StreamEvent) bool {
	switch ev.Kind {
	case KindNewHead:
		// CAS-retry on the packed (num, hash) pointer. On the happy path
		// exactly one goroutine per distinct head wins the swap and falls
		// through to mark+deliver; any concurrent ingest of the same head
		// sees its CAS fail, reloads, and drops as a dupe on the next
		// iteration. Reorgs at the same height (same num, different hash)
		// win a second CAS and are delivered.
		next := &headMarker{num: ev.Block.Number, hash: ev.Block.Hash}
		for {
			prev := ns.lastHead.Load()
			if prev != nil {
				if ev.Block.Number < prev.num {
					return false
				}
				if ev.Block.Number == prev.num && prev.hash == ev.Block.Hash {
					return false
				}
			}
			if ns.lastHead.CompareAndSwap(prev, next) {
				break
			}
		}
		// Store in the fallback window too for the rare "older-but-not-
		// newest" case (out-of-order delivery on a reorg boundary).
		ns.headFallback.Mark(ev.Block.Hash)
		return true
	case KindLog, KindPendingTx:
		ns.filterMu.RLock()
		win := ns.filterDedup[ev.FilterHash]
		ns.filterMu.RUnlock()
		if win == nil {
			// Filter not registered with this indexer instance (e.g. an
			// ingress delivered an event for a filter we never EnsureFilter'd).
			// Allow through — upstream subs we didn't request are rare.
			return true
		}
		key := DedupKeyForFilter(ev.Kind.String(), ev.Payload)
		if key == "" {
			// Couldn't extract a key; don't pretend we deduped.
			return true
		}
		return win.Mark(key)
	default:
		return true
	}
}

// classify computes Lifecycle for an event using the network's finality
// depth. Events at or below (latest - depth) are considered finalized.
// Applied only to events with a BlockRef; KindPendingTx and KindReorg
// default to LifeSoft.
func (i *Indexer) classify(ns *networkState, ev StreamEvent) Lifecycle {
	if ev.Block.Zero() {
		return LifeSoft
	}
	depth := ns.handle.FinalityDepth()
	if depth <= 0 {
		return LifeSoft
	}
	var latest int64
	if head := ns.lastHead.Load(); head != nil {
		latest = head.num
	}
	if latest-ev.Block.Number >= depth {
		return LifeFinalized
	}
	return LifeSoft
}

// fanOut dispatches to every registered egress whose InterestedIn matches.
func (i *Indexer) fanOut(ev IndexedEvent) {
	i.egresses.Range(func(_, v any) bool {
		eg := v.(EventEgress)
		if !eg.InterestedIn(ev.Kind, ev.NetworkId, ev.FilterHash) {
			return true
		}
		eg.Deliver(ev)
		return true
	})
}
