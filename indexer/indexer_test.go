package indexer

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// --- fakes -----------------------------------------------------------

type fakeNetwork struct {
	id string

	mu          sync.Mutex
	undelivered int
}

func newFakeNetwork(id string) *fakeNetwork {
	return &fakeNetwork{id: id}
}

func (n *fakeNetwork) Id() string             { return n.id }
func (n *fakeNetwork) DedupDropped(EventKind) {}
func (n *fakeNetwork) DedupEntriesAdded(int)  {}
func (n *fakeNetwork) Undelivered(EventKind) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.undelivered++
}
func (n *fakeNetwork) undeliveredCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.undelivered
}

type fakeEgress struct {
	name    string
	filters map[string]struct{} // filterHash -> interested
	// hook, when set, runs at the start of every Deliver.
	hook func(IndexedEvent)

	mu       sync.Mutex
	received []IndexedEvent
}

func (e *fakeEgress) Name() string { return e.name }
func (e *fakeEgress) InterestedIn(kind EventKind, networkId, filterHash string) bool {
	_, ok := e.filters[filterHash]
	return ok
}
func (e *fakeEgress) Deliver(ev IndexedEvent) {
	if e.hook != nil {
		e.hook(ev)
	}
	e.mu.Lock()
	e.received = append(e.received, ev)
	e.mu.Unlock()
}
func (e *fakeEgress) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.received)
}

type fakeIngress struct {
	name           string
	ensureCalls    atomic.Int32
	removeCalls    atomic.Int32
	lastParamsHash atomic.Value // string
	// ensureErr, when set, is returned from every EnsureFilter call.
	errMu     sync.Mutex
	ensureErr error
	// active reports whether the ingress holds the filter. Like the WS
	// adapter, a failed EnsureFilter still stores it.
	active atomic.Bool
	// hook, when set, runs at the start of every "ensure"/"remove" call.
	hook func(op string)

	startedFor NetworkHandle
	sink       Sink
}

func (i *fakeIngress) Name() string { return i.name }
func (i *fakeIngress) Start(_ context.Context, nw NetworkHandle, sink Sink) error {
	i.startedFor = nw
	i.sink = sink
	return nil
}
func (i *fakeIngress) setErr(err error) {
	i.errMu.Lock()
	i.ensureErr = err
	i.errMu.Unlock()
}
func (i *fakeIngress) getErr() error {
	i.errMu.Lock()
	defer i.errMu.Unlock()
	return i.ensureErr
}
func (i *fakeIngress) EnsureFilter(_ context.Context, _ string, paramsHash string, _ []interface{}) error {
	if i.hook != nil {
		i.hook("ensure")
	}
	i.ensureCalls.Add(1)
	i.lastParamsHash.Store(paramsHash)
	i.active.Store(true)
	return i.getErr()
}
func (i *fakeIngress) FilterLive(_, _ string) bool {
	return i.active.Load() && i.getErr() == nil
}
func (i *fakeIngress) RemoveFilter(_ context.Context, _, _ string) error {
	if i.hook != nil {
		i.hook("remove")
	}
	i.removeCalls.Add(1)
	i.active.Store(false)
	return nil
}

// fakeSelector is a static IngressSelector for tests.
type fakeSelector struct {
	defaults  []string
	fallbacks []string
}

func (s *fakeSelector) Select(_, _ string, _ []interface{}) ([]string, []string) {
	return s.defaults, s.fallbacks
}

// --- tests -----------------------------------------------------------

func newIndexer(t *testing.T) *Indexer {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t))
	return New(&logger, Options{})
}

func TestIndexer_ReportsUndeliveredEvents(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:1")
	idx.RegisterNetwork(nw)

	// A pending transaction for a filter no client holds.
	idx.Ingest(StreamEvent{Kind: KindPendingTx, NetworkId: "evm:1", SourceId: "ws:up1", FilterHash: "orphan",
		Payload: json.RawMessage(`"0xbeef"`)})
	if got := nw.undeliveredCount(); got != 1 {
		t.Fatalf("want 1 undelivered event, got %d", got)
	}
}

// A subscriber arriving while the last one's teardown is still talking to
// the ingress must end up subscribed, not torn down by the late removal.
func TestIndexer_Filter_EnsureDuringReleaseStaysSubscribed(t *testing.T) {
	ctx := context.Background()
	idx := newIndexer(t)
	idx.RegisterNetwork(newFakeNetwork("evm:1"))
	removing, unblock := make(chan struct{}), make(chan struct{})
	ing := &fakeIngress{name: "a", hook: func(op string) {
		if op == "remove" {
			close(removing)
			<-unblock
		}
	}}
	if err := idx.AddIngress(ctx, "evm:1", ing); err != nil {
		t.Fatal(err)
	}
	params := []interface{}{"logs"}
	h, err := idx.EnsureFilter(ctx, "evm:1", "logs", params)
	if err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	go func() {
		idx.ReleaseFilter(ctx, "evm:1", "logs", h)
		close(released)
	}()
	<-removing
	ensured := make(chan error, 1)
	go func() {
		_, err := idx.EnsureFilter(ctx, "evm:1", "logs", params)
		ensured <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the new subscriber reach the indexer
	close(unblock)
	<-released
	if err := <-ensured; err != nil {
		t.Fatal(err)
	}
	if !ing.active.Load() {
		t.Fatal("subscriber that arrived during teardown must end up subscribed")
	}
}

// A subscriber that joins while the first subscribe is still in flight must
// not be handed a subscription when that subscribe fails, and the failure
// must not leave a refcount behind that makes later subscribers skip the
// ingress.
func TestIndexer_Filter_FailedEnsureIsNotShared(t *testing.T) {
	ctx := context.Background()
	idx := newIndexer(t)
	idx.RegisterNetwork(newFakeNetwork("evm:1"))
	ensuring, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ing := &fakeIngress{name: "a", hook: func(op string) {
		if op == "ensure" && calls.Add(1) == 1 {
			close(ensuring)
			<-unblock
		}
	}}
	ing.setErr(errTest("down"))
	if err := idx.AddIngress(ctx, "evm:1", ing); err != nil {
		t.Fatal(err)
	}
	params := []interface{}{"logs"}

	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := idx.EnsureFilter(ctx, "evm:1", "logs", params)
		first <- err
	}()
	<-ensuring
	go func() {
		_, err := idx.EnsureFilter(ctx, "evm:1", "logs", params)
		second <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the second subscriber reach the indexer
	close(unblock)
	if <-first == nil {
		t.Fatal("first subscribe must fail")
	}
	if <-second == nil {
		t.Fatal("joining a failed subscribe must not return a silent subscription")
	}

	ing.setErr(nil)
	before := ing.ensureCalls.Load()
	if _, err := idx.EnsureFilter(ctx, "evm:1", "logs", params); err != nil {
		t.Fatal(err)
	}
	if ing.ensureCalls.Load() != before+1 {
		t.Fatal("after a failed subscribe the next subscriber must subscribe on the ingress")
	}
}

func TestIndexer_Filter_ConcurrentEnsureReleaseConverges(t *testing.T) {
	ctx := context.Background()
	idx := newIndexer(t)
	idx.RegisterNetwork(newFakeNetwork("evm:1"))
	ing := &fakeIngress{name: "a"}
	if err := idx.AddIngress(ctx, "evm:1", ing); err != nil {
		t.Fatal(err)
	}
	params := []interface{}{"logs"}

	var wg sync.WaitGroup
	for n := 0; n < 50; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := idx.EnsureFilter(ctx, "evm:1", "logs", params)
			if err != nil {
				t.Error(err)
				return
			}
			idx.ReleaseFilter(ctx, "evm:1", "logs", h)
		}()
	}
	wg.Wait()
	if ing.active.Load() {
		t.Fatal("every client released; the filter must be torn down")
	}
	if _, err := idx.EnsureFilter(ctx, "evm:1", "logs", params); err != nil {
		t.Fatal(err)
	}
	if !ing.active.Load() {
		t.Fatal("a new client must subscribe the filter again")
	}
}

// An ingress may keep a filter whose subscribe failed (to retry on
// reconnect). A failed EnsureFilter must remove it again.
func TestIndexer_Filter_FailedEnsureRemovesFromIngress(t *testing.T) {
	idx := newIndexer(t)
	a, b, _ := registerThreeIngresses(t, idx)
	a.setErr(errTest("a"))
	b.setErr(errTest("b"))
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a"}, fallbacks: []string{"b"}})

	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err == nil {
		t.Fatal("expected error")
	}
	if a.active.Load() || b.active.Load() {
		t.Fatalf("failed subscribe left filters behind: a=%v b=%v", a.active.Load(), b.active.Load())
	}
}

func TestIndexer_UnregisteredNetwork(t *testing.T) {
	idx := newIndexer(t)
	if err := idx.AddIngress(context.Background(), "evm:missing", &fakeIngress{name: "x"}); err == nil {
		t.Fatal("expected error on unregistered network")
	}
	if _, err := idx.EnsureFilter(context.Background(), "evm:missing", "logs", []interface{}{"logs"}); err == nil {
		t.Fatal("expected error on unregistered network")
	}
}

// registerThreeIngresses attaches three fakeIngresses named a, b, c on
// "evm:1" and returns them. Helper for selector tests.
func registerThreeIngresses(t *testing.T, idx *Indexer) (a, b, c *fakeIngress) {
	t.Helper()
	idx.RegisterNetwork(newFakeNetwork("evm:1"))
	a = &fakeIngress{name: "a"}
	b = &fakeIngress{name: "b"}
	c = &fakeIngress{name: "c"}
	for _, ing := range []*fakeIngress{a, b, c} {
		if err := idx.AddIngress(context.Background(), "evm:1", ing); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestIndexer_Selector_DefaultsOnlyHappyPath(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}})

	_, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ensureCalls.Load() != 1 || b.ensureCalls.Load() != 1 {
		t.Fatalf("defaults must be called once each, got a=%d b=%d", a.ensureCalls.Load(), b.ensureCalls.Load())
	}
	if c.ensureCalls.Load() != 0 {
		t.Fatalf("fallback must not be touched when defaults succeed, got %d calls", c.ensureCalls.Load())
	}
}

func TestIndexer_Selector_PartialDefaultFailureReturnsNil(t *testing.T) {
	idx := newIndexer(t)
	a, _, c := registerThreeIngresses(t, idx)
	a.setErr(errTest("a is sad"))
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}})

	_, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err != nil {
		t.Fatalf("partial failure must not surface: %v", err)
	}
	if a.ensureCalls.Load() != 1 {
		t.Fatalf("failing default must still be attempted once, got %d", a.ensureCalls.Load())
	}
	if c.ensureCalls.Load() != 0 {
		t.Fatalf("fallback must not be touched when at least one default succeeded, got %d", c.ensureCalls.Load())
	}
}

func TestIndexer_Selector_AllDefaultsFailEscalatesToFallback(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	a.setErr(errTest("a"))
	b.setErr(errTest("b"))
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}})

	_, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err != nil {
		t.Fatalf("escalation should return nil when fallback succeeds: %v", err)
	}
	if c.ensureCalls.Load() != 1 {
		t.Fatalf("fallback must be tried after all defaults fail, got %d calls", c.ensureCalls.Load())
	}
}

// The selector excludes an ingress (e.g. a down upstream) by not naming it.
// An unnamed ingress must not be subscribed, and must not mask a failed
// default tier.
func TestIndexer_Selector_UnnamedIngressIsExcluded(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	a.setErr(errTest("a"))
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a"}, fallbacks: []string{"b"}})

	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err != nil {
		t.Fatalf("fallback should have carried the filter: %v", err)
	}
	if c.ensureCalls.Load() != 0 {
		t.Fatalf("unnamed ingress must not be subscribed, got %d calls", c.ensureCalls.Load())
	}
	if b.ensureCalls.Load() != 1 {
		t.Fatalf("fallback must be tried when every default failed, got %d calls", b.ensureCalls.Load())
	}
}

// reconcileNow runs a reconcile of evm:1 synchronously.
func reconcileNow(t *testing.T, idx *Indexer) {
	t.Helper()
	nsRaw, ok := idx.networks.Load("evm:1")
	if !ok {
		t.Fatal("evm:1 not registered")
	}
	idx.reconcile(context.Background(), nsRaw.(*networkState))
}

// The fallbacks carry a filter only while no default has it live: they take
// over when the defaults lose it, and hand it back once one has it again.
func TestIndexer_Reconcile_FallbackTakesOverAndHandsBack(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}})
	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err != nil {
		t.Fatal(err)
	}

	reconcileNow(t, idx)
	if c.ensureCalls.Load() != 0 {
		t.Fatalf("fallback must stay idle while a default is live, got %d calls", c.ensureCalls.Load())
	}

	a.setErr(errTest("a lost it"))
	b.setErr(errTest("b lost it"))
	reconcileNow(t, idx)
	if c.ensureCalls.Load() != 1 || !c.FilterLive("logs", "") {
		t.Fatalf("fallback must take over once no default is live, got %d calls", c.ensureCalls.Load())
	}
	if a.removeCalls.Load() != 0 || b.removeCalls.Load() != 0 {
		t.Fatal("defaults keep the filter, to retry it")
	}

	reconcileNow(t, idx)
	if c.ensureCalls.Load() != 1 {
		t.Fatalf("an ingress already carrying the filter must not be asked again, got %d", c.ensureCalls.Load())
	}

	b.setErr(nil)
	reconcileNow(t, idx)
	if c.removeCalls.Load() != 1 || c.active.Load() {
		t.Fatalf("fallback must hand the filter back once a default is live, removes=%d", c.removeCalls.Load())
	}
}

// A default the selector no longer names (e.g. the policy excluded it) gives
// the filter up; if that leaves no default, the fallbacks take it.
func TestIndexer_Reconcile_FollowsSelector(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	sel := &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}}
	idx.RegisterNetworkSelector("evm:1", sel)
	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err != nil {
		t.Fatal(err)
	}

	sel.defaults = []string{"b"}
	reconcileNow(t, idx)
	if a.removeCalls.Load() != 1 || c.ensureCalls.Load() != 0 {
		t.Fatalf("a must give the filter up while b still carries it: a.removes=%d c.ensures=%d",
			a.removeCalls.Load(), c.ensureCalls.Load())
	}

	sel.defaults = nil
	reconcileNow(t, idx)
	if b.removeCalls.Load() != 1 || c.ensureCalls.Load() != 1 {
		t.Fatalf("with no default left the fallback must carry it: b.removes=%d c.ensures=%d",
			b.removeCalls.Load(), c.ensureCalls.Load())
	}

	// Returning defaults take it back; the fallback keeps it until one of
	// them has it live, so there is no gap.
	sel.defaults = []string{"a", "b"}
	reconcileNow(t, idx)
	if a.ensureCalls.Load() != 2 || b.ensureCalls.Load() != 2 || c.removeCalls.Load() != 0 {
		t.Fatalf("returning defaults must take it back alongside the fallback: a=%d b=%d c.removes=%d",
			a.ensureCalls.Load(), b.ensureCalls.Load(), c.removeCalls.Load())
	}
	reconcileNow(t, idx)
	if c.removeCalls.Load() != 1 {
		t.Fatalf("the fallback must hand back once a default is live, removes=%d", c.removeCalls.Load())
	}
}

// Every head triggers the recheck, and a released filter is left alone.
func TestIndexer_Reconcile_RunsOnHeadsAndSkipsReleased(t *testing.T) {
	idx := newIndexer(t)
	a, _, c := registerThreeIngresses(t, idx)
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a"}, fallbacks: []string{"c"}})
	h, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err != nil {
		t.Fatal(err)
	}
	a.setErr(errTest("a lost it"))

	idx.Ingest(StreamEvent{Kind: KindNewHead, NetworkId: "evm:1", SourceId: "ws:c"})
	deadline := time.Now().Add(2 * time.Second)
	for c.ensureCalls.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("a head must trigger the recheck")
		}
		time.Sleep(5 * time.Millisecond)
	}

	nsRaw, _ := idx.networks.Load("evm:1")
	ns := nsRaw.(*networkState)
	for ns.reconciling.Load() {
		time.Sleep(time.Millisecond)
	}
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h)
	reconcileNow(t, idx)
	if a.ensureCalls.Load() != 1 || c.ensureCalls.Load() != 1 {
		t.Fatalf("a released filter must not be resubscribed: a=%d c=%d", a.ensureCalls.Load(), c.ensureCalls.Load())
	}
}

func TestIndexer_Selector_NothingSelectedFails(t *testing.T) {
	idx := newIndexer(t)
	registerThreeIngresses(t, idx)
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{})

	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err == nil {
		t.Fatal("a filter with no ingress behind it must fail, not return a silent subscription")
	}
}

func TestIndexer_Selector_AllFailReturnsJoinedError(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	a.setErr(errTest("a"))
	b.setErr(errTest("b"))
	c.setErr(errTest("c"))
	idx.RegisterNetworkSelector("evm:1", &fakeSelector{defaults: []string{"a", "b"}, fallbacks: []string{"c"}})

	_, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err == nil {
		t.Fatal("expected joined error when every tier fails")
	}
	msg := err.Error()
	for _, want := range []string{"a", "b", "c"} {
		if !containsStr(msg, want) {
			t.Fatalf("joined error must include %q, got %q", want, msg)
		}
	}
	// Re-attempt after fixing the ingresses must work — dedup window and
	// refcount must have been rolled back.
	a.setErr(nil)
	b.setErr(nil)
	c.setErr(nil)
	if _, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"}); err != nil {
		t.Fatalf("second attempt must succeed after errors clear, got %v", err)
	}
}

func TestIndexer_NilSelector_FansOutToAllIngresses(t *testing.T) {
	idx := newIndexer(t)
	a, b, c := registerThreeIngresses(t, idx)
	// No selector registered: every ingress must be tried.

	h, err := idx.EnsureFilter(context.Background(), "evm:1", "logs", []interface{}{"logs"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ing := range []*fakeIngress{a, b, c} {
		if ing.ensureCalls.Load() != 1 {
			t.Fatalf("ingress %q: want 1 EnsureFilter call, got %d", ing.name, ing.ensureCalls.Load())
		}
	}
	idx.ReleaseFilter(context.Background(), "evm:1", "logs", h)
	for _, ing := range []*fakeIngress{a, b, c} {
		if ing.removeCalls.Load() != 1 {
			t.Fatalf("ingress %q: want 1 RemoveFilter call, got %d", ing.name, ing.removeCalls.Load())
		}
	}
}

// Test-only helpers.

type errTest string

func (e errTest) Error() string { return string(e) }

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, substr string) int {
	n := len(substr)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == substr {
			return i
		}
	}
	return -1
}
