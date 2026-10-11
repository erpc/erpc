package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// Options configures an Indexer. Zero values select the defaults.
type Options struct {
	// DedupWindowSize is the per-filter dedup capacity.
	DedupWindowSize int
}

// Indexer dedupes the StreamEvents ingresses push at it and fans them out
// to every interested egress. Ingest runs synchronously on the ingress's
// goroutine, which keeps per-source ordering without extra queues.
type Indexer struct {
	logger *zerolog.Logger
	opts   Options

	networks sync.Map // networkId -> *networkState
	egresses sync.Map // egress Name() -> EventEgress
}

// headMarker is the most recently delivered head of a network.
type headMarker struct {
	num  int64
	hash string
}

// networkState holds a network's NetworkHandle, the ingresses feeding it,
// the newHeads dedup marker, and the dedup windows for each active filter.
type networkState struct {
	handle NetworkHandle

	ingressMu sync.RWMutex
	ingresses map[string]EventIngress // Name() -> ingress
	selector  IngressSelector         // nil: every ingress is a default

	// headMu serialises newHeads dedup and fan-out, so concurrent sources
	// can neither deliver the same head twice nor deliver heads out of
	// order. Egress Deliver is non-blocking by contract.
	headMu   sync.Mutex
	lastHead *headMarker // guarded by headMu; nil until the first head

	filterMu sync.RWMutex
	filters  map[string]*filterState // paramsHash -> state

	// reconciling is set while a reconcile of the filters runs.
	reconciling atomic.Bool
}

// filterState is one filter subscription shared by every client with the
// same params.
type filterState struct {
	// mu is held across this filter's ingress calls so a subscribe and the
	// last client's teardown never interleave.
	mu         sync.Mutex
	subscribed bool // guarded by mu
	refs       int  // guarded by networkState.filterMu
	dedup      *DedupWindow

	// subType and params are the filter's; on names the ingresses it was
	// put on and not removed from since. Guarded by mu.
	subType string
	params  []interface{}
	on      map[string]struct{}
}

// New returns an empty Indexer. Events for networks not registered with
// RegisterNetwork are ignored.
func New(logger *zerolog.Logger, opts Options) *Indexer {
	return &Indexer{
		logger: logger,
		opts:   opts,
	}
}

// RegisterNetwork registers a network; later calls for the same Id() keep
// the first handle.
func (i *Indexer) RegisterNetwork(nw NetworkHandle) {
	if _, ok := i.networks.Load(nw.Id()); ok {
		return
	}
	i.networks.LoadOrStore(nw.Id(), &networkState{
		handle:    nw,
		ingresses: make(map[string]EventIngress),
		filters:   make(map[string]*filterState),
	})
}

func errNetworkNotRegistered(networkId string) error {
	return fmt.Errorf("indexer: network %q not registered", networkId)
}

// AddIngress adds an ingress to a registered network and starts it with
// the indexer as its Sink.
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

// Attach registers an egress until the returned detach is called.
func (i *Indexer) Attach(eg EventEgress) (detach func()) {
	i.egresses.Store(eg.Name(), eg)
	return func() { i.egresses.Delete(eg.Name()) }
}

// RegisterNetworkSelector sets the IngressSelector EnsureFilter uses for a
// registered network; nil selects every ingress.
func (i *Indexer) RegisterNetworkSelector(networkId string, sel IngressSelector) {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return
	}
	ns := nsRaw.(*networkState)
	ns.ingressMu.Lock()
	ns.selector = sel
	ns.ingressMu.Unlock()
}

// EnsureFilter takes a reference on a filter, subscribing it on the
// network's ingresses if needed. Concurrent callers for the same filter wait
// for the subscribe in flight and retry it themselves if it failed.
func (i *Indexer) EnsureFilter(ctx context.Context, networkId, subType string, params []interface{}) (paramsHash string, err error) {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return "", errNetworkNotRegistered(networkId)
	}
	ns := nsRaw.(*networkState)
	paramsHash = BuildParamsKey(params)

	ns.filterMu.Lock()
	f := ns.filters[paramsHash]
	if f == nil {
		f = &filterState{dedup: NewDedupWindow(i.opts.DedupWindowSize)}
		ns.filters[paramsHash] = f
	}
	f.refs++
	ns.filterMu.Unlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribed {
		return paramsHash, nil
	}
	f.subType, f.params, f.on = subType, params, make(map[string]struct{})
	if err := i.subscribe(ctx, ns, f, paramsHash); err != nil {
		// Ingresses may keep a filter whose subscribe failed (to retry on
		// reconnect), so remove it everywhere.
		i.removeFromIngresses(ctx, ns, subType, paramsHash)
		ns.filterMu.Lock()
		f.refs--
		if f.refs == 0 {
			delete(ns.filters, paramsHash)
		}
		ns.filterMu.Unlock()
		return paramsHash, err
	}
	f.subscribed = true
	return paramsHash, nil
}

// subscribe puts a new filter on the selector's defaults, and on its
// fallbacks if no default took it. It fails when no ingress subscribed,
// including when none was selected. An ingress that failed keeps retrying
// the filter until it is removed. The caller holds f.mu.
func (i *Indexer) subscribe(ctx context.Context, ns *networkState, f *filterState, paramsHash string) error {
	networkId := ns.handle.Id()
	ings, sel := ns.snapshotIngresses()
	defaults, fallbacks := partitionIngresses(sel, ings, networkId, f.subType, f.params)

	var errs []error
	for _, tier := range [][]EventIngress{defaults, fallbacks} {
		subscribed := false
		for _, ing := range tier {
			f.on[ing.Name()] = struct{}{}
			if err := ing.EnsureFilter(ctx, f.subType, paramsHash, f.params); err != nil {
				errs = append(errs, err)
				i.logger.Warn().Err(err).Str("ingress", ing.Name()).Str("networkId", networkId).
					Str("subType", f.subType).Str("paramsHash", paramsHash).
					Msg("ingress EnsureFilter failed")
				continue
			}
			subscribed = true
		}
		if subscribed {
			return nil
		}
	}
	if len(errs) == 0 {
		return fmt.Errorf("indexer: no ingress selected for %s filter on network %q", f.subType, networkId)
	}
	return errors.Join(errs...)
}

// snapshotIngresses copies the network's ingresses and selector.
func (ns *networkState) snapshotIngresses() (map[string]EventIngress, IngressSelector) {
	ns.ingressMu.RLock()
	defer ns.ingressMu.RUnlock()
	ings := make(map[string]EventIngress, len(ns.ingresses))
	for name, ing := range ns.ingresses {
		ings[name] = ing
	}
	return ings, ns.selector
}

// triggerReconcile starts a reconcile of ns's filters unless one is running
// or there are none. Triggers it skips are covered by the next one.
func (i *Indexer) triggerReconcile(ns *networkState) {
	ns.filterMu.RLock()
	none := len(ns.filters) == 0
	ns.filterMu.RUnlock()
	if none || !ns.reconciling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer ns.reconciling.Store(false)
		i.reconcile(context.Background(), ns)
	}()
}

// reconcile moves every subscribed filter of ns onto the ingresses the
// selector picks now: all its defaults, plus its fallbacks while no default
// has the filter live; it is removed from every other ingress it is on.
func (i *Indexer) reconcile(ctx context.Context, ns *networkState) {
	ns.filterMu.RLock()
	filters := make(map[string]*filterState, len(ns.filters))
	for hash, f := range ns.filters {
		filters[hash] = f
	}
	ns.filterMu.RUnlock()
	ings, sel := ns.snapshotIngresses()

	for hash, f := range filters {
		f.mu.Lock()
		if f.subscribed {
			i.reconcileFilter(ctx, ns, ings, sel, hash, f)
		}
		f.mu.Unlock()
	}
}

// reconcileFilter is reconcile for one filter; the caller holds f.mu.
func (i *Indexer) reconcileFilter(ctx context.Context, ns *networkState, ings map[string]EventIngress, sel IngressSelector, hash string, f *filterState) {
	defaults, fallbacks := partitionIngresses(sel, ings, ns.handle.Id(), f.subType, f.params)
	want := make(map[string]EventIngress, len(defaults)+len(fallbacks))
	covered := false
	for _, ing := range defaults {
		want[ing.Name()] = ing
		covered = covered || ing.FilterLive(f.subType, hash)
	}
	if !covered {
		for _, ing := range fallbacks {
			want[ing.Name()] = ing
		}
	}

	for name, ing := range want {
		if _, ok := f.on[name]; ok {
			continue
		}
		f.on[name] = struct{}{}
		if err := ing.EnsureFilter(ctx, f.subType, hash, f.params); err != nil {
			i.logger.Warn().Err(err).Str("ingress", name).Str("networkId", ns.handle.Id()).
				Str("subType", f.subType).Str("paramsHash", hash).
				Msg("ingress EnsureFilter failed, it keeps retrying")
		}
	}
	for name := range f.on {
		if _, ok := want[name]; ok {
			continue
		}
		delete(f.on, name)
		if ing := ings[name]; ing != nil {
			if err := ing.RemoveFilter(ctx, f.subType, hash); err != nil {
				i.logger.Warn().Err(err).Str("ingress", name).Str("networkId", ns.handle.Id()).
					Str("subType", f.subType).Str("paramsHash", hash).
					Msg("ingress RemoveFilter failed")
			}
		}
	}
}

// partitionIngresses resolves the selector's tiers to registered ingresses,
// dropping unknown and repeated names. Without a selector every ingress is
// a default.
func partitionIngresses(sel IngressSelector, ings map[string]EventIngress, networkId, subType string, params []interface{}) (defaults, fallbacks []EventIngress) {
	if sel == nil {
		defaults = make([]EventIngress, 0, len(ings))
		for _, ing := range ings {
			defaults = append(defaults, ing)
		}
		return defaults, nil
	}
	dNames, fNames := sel.Select(networkId, subType, params)
	named := make(map[string]struct{}, len(dNames)+len(fNames))
	pick := func(names []string) []EventIngress {
		out := make([]EventIngress, 0, len(names))
		for _, n := range names {
			if _, dup := named[n]; dup {
				continue
			}
			named[n] = struct{}{}
			if ing, ok := ings[n]; ok {
				out = append(out, ing)
			}
		}
		return out
	}
	return pick(dNames), pick(fNames)
}

// ReleaseFilter drops a reference taken by EnsureFilter and unsubscribes the
// filter from every ingress once none remain.
func (i *Indexer) ReleaseFilter(ctx context.Context, networkId, subType, paramsHash string) {
	nsRaw, ok := i.networks.Load(networkId)
	if !ok {
		return
	}
	ns := nsRaw.(*networkState)

	ns.filterMu.Lock()
	f := ns.filters[paramsHash]
	if f == nil || f.refs <= 0 {
		ns.filterMu.Unlock()
		return
	}
	f.refs--
	last := f.refs == 0
	ns.filterMu.Unlock()
	if !last {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ns.filterMu.Lock()
	// While waiting for f.mu a new client may have joined, or an earlier
	// release already tore the filter down.
	keep := f.refs > 0 || !f.subscribed
	ns.filterMu.Unlock()
	if keep {
		return
	}
	i.removeFromIngresses(ctx, ns, subType, paramsHash)
	f.subscribed, f.on = false, nil
	ns.filterMu.Lock()
	if f.refs == 0 {
		delete(ns.filters, paramsHash)
	}
	ns.filterMu.Unlock()
}

// removeFromIngresses calls RemoveFilter on every ingress; those that never
// subscribed the filter no-op.
func (i *Indexer) removeFromIngresses(ctx context.Context, ns *networkState, subType, paramsHash string) {
	ns.ingressMu.RLock()
	ings := make([]EventIngress, 0, len(ns.ingresses))
	for _, ing := range ns.ingresses {
		ings = append(ings, ing)
	}
	ns.ingressMu.RUnlock()

	for _, ing := range ings {
		if err := ing.RemoveFilter(ctx, subType, paramsHash); err != nil {
			i.logger.Warn().Err(err).Str("ingress", ing.Name()).Str("networkId", ns.handle.Id()).
				Str("subType", subType).Str("paramsHash", paramsHash).
				Msg("ingress RemoveFilter failed")
		}
	}
}

// Ingest reports heads to the NetworkHandle, dedupes, and fans out to every
// interested egress.
func (i *Indexer) Ingest(ev StreamEvent) {
	nsRaw, ok := i.networks.Load(ev.NetworkId)
	if !ok {
		return
	}
	ns := nsRaw.(*networkState)

	// Each head, delivered or not, rechecks where the filters belong.
	if ev.Kind == KindNewHead {
		i.triggerReconcile(ns)
	}

	// Before dedup, so every source's head counts even if another source
	// already delivered it.
	if ev.Kind == KindNewHead && !ev.Block.Zero() && ev.SourceId != "" {
		if !ns.handle.SuggestLatestBlock(ev.SourceId, ev.Block.Number) {
			return
		}
	}

	// The upstream's removed flag is trusted as-is.
	removed := ev.Kind == KindLog && logRemoved(ev.Payload)

	if ev.Kind == KindNewHead {
		ns.headMu.Lock()
		defer ns.headMu.Unlock()
	}
	if !i.dedupe(ns, &ev, removed) {
		return
	}

	i.fanOut(IndexedEvent{StreamEvent: ev, Removed: removed})
}

// dedupe reports whether ev should be delivered. Heads are compared to the
// last delivered head; filter events go through their filter's DedupWindow.
func (i *Indexer) dedupe(ns *networkState, ev *StreamEvent, removed bool) bool {
	switch ev.Kind {
	case KindNewHead:
		// Caller holds headMu. A different hash at the same height is a
		// reorg and is delivered.
		if prev := ns.lastHead; prev != nil {
			if ev.Block.Number < prev.num || (ev.Block.Number == prev.num && strings.EqualFold(prev.hash, ev.Block.Hash)) {
				return false
			}
		}
		ns.lastHead = &headMarker{num: ev.Block.Number, hash: strings.Clone(ev.Block.Hash)}
		return true
	case KindLog, KindPendingTx:
		ns.filterMu.RLock()
		f := ns.filters[ev.FilterHash]
		ns.filterMu.RUnlock()
		if f == nil {
			return true
		}
		key := DedupKeyForFilter(ev.Kind.String(), ev.Payload)
		if key == "" {
			return true
		}
		return f.dedup.Mark(key, removed)
	default:
		return true
	}
}

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
