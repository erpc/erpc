package realworld

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// vendorSpec is one simulated upstream of a scenario network.
type vendorSpec struct {
	key  string // ws-a, http-b, …
	name string // vendorName
	ws   bool
}

// baseVendors are baseline B: 2 wss and 2 https upstreams over 2 vendors.
var baseVendors = []vendorSpec{
	{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true},
	{"http-a", "vendor-a", false}, {"http-b", "vendor-b", false},
}

// spec is one catalogue scenario.
type spec struct {
	name      string
	project   string // default "rw"
	vendors   []vendorSpec
	integrity bool
	// integrityOff are pods whose config leaves integrity off when
	// integrity is on (K5: the same chain and plan, on vs off); checks are
	// integrity checks enabled beyond the level's.
	integrityOff []int
	checks       []string
	minimal      bool
	// late: vendor keys whose upstreams register only once the network
	// serves on every pod; their heads vote from the first one.
	late []string
	// pods is how many pods the phase boots (0 is 2); memory: no redis,
	// each pod keeps its shared state in memory; oldPods run the old
	// image (X7); cold pods boot only when the body starts them (X3);
	// webSocket is merged into every pod's server.webSocket (D7). A phase
	// reads its first spec's.
	pods      int
	memory    bool
	oldPods   []int
	cold      []int
	webSocket map[string]interface{}
	// long: the phase runs in the long tier only (selectedPhases), as a
	// row whose own timeline takes more than a few minutes.
	long bool
	// sim configures the chain; ChainID, unless set, and Seed come from the
	// phase (P11-nitro needs a Nitro chain's id), a zero BlockTime is
	// blockTime.
	sim chainsim.Config
	// faults are vendor faults set before any pod boots, by vendor key.
	faults map[string]chainsim.Faults
	// tags are upstream tags by vendor key; upEvm are keys merged into an
	// upstream's evm block, up keys merged into its entry, budgets its
	// rateLimitBudget in requests per second on each pod, all by vendor
	// key; evm and network are merged into the network's evm block and
	// network entry; cache gives the network an evmJsonRpcCache with a
	// permanent and a recent connector (cacheFinal, cacheRecent).
	tags    map[string][]string
	upEvm   map[string]map[string]interface{}
	up      map[string]map[string]interface{}
	budgets map[string]uint32
	evm     map[string]interface{}
	network map[string]interface{}
	cache   bool
	// vars shorten eRPC duration variables, by name
	// (testdata/realworld_vars.go), on every pod of the scenario's phase.
	vars map[string]time.Duration
	// headless are pods the oracle never asks eth_blockNumber, so its reads
	// never make their pollers fetch a head; numbered pods are headless and
	// get numbered calls only: no by-hash, logs by number.
	headless, numbered []int
	// idle: no client request reaches a pod before the body; waitServing
	// waits for its confirmed head by scrape alone (Z3: history starts at
	// the first client request).
	idle bool
	// onHead runs for each header a newHeads client receives.
	onHead func(s *scenario, c *headsClient, pod int, n uint64, h hash)
	// onDeliver runs for each block any client receives: a header of
	// newHeads or StreamBlocks, the block of a log.
	onDeliver func(s *scenario, c *clientBase, pod int, b *chainsim.Block)
	// bound is the delivery bound for clients that read (§ per scenario).
	bound time.Duration
	// allow names the ends and jumps the scenario's faults may cause.
	allow []string
	// clients opens the scenario's clients; nil opens baseline B's.
	clients func(s *scenario)
	body    func(s *scenario)
	// after runs once the clients stopped, before the verdict (K5
	// compares them).
	after func(s *scenario)
}

// scenario is one spec running on its own chain id.
type scenario struct {
	spec
	t       *testing.T
	h       *harness
	chain   *chainsim.Chain
	vendors map[string]*chainsim.Vendor
	proxies map[string]*proxy
	net     *netSpec
	dir     string
	// rpcs are HTTP clients per pod, by use-upstream selector ("" none).
	rpcs  map[string][]*rpc.Client
	httpc *http.Client
	tr    *transcript
	// graceServed counts reads the minority grace allowed (U18).
	graceServed atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	checks sync.WaitGroup

	mu         sync.Mutex
	violations []string
	excuses    []*excuse
	haltAt     time.Time
	heads      []*headsClient
	logs       []*logsClient
	grpcs      []*grpcClient
	reorgs     []reorg
	memo       map[string]*memoEntry
	allowSet   map[string]bool
	// fork is the first block of a minority fork the scenario runs, nil
	// none: an HTTP read on a pod may answer a minority-fork block until W,
	// plus the tracker's processing, after a fork block first reached the
	// pod as a latest answer, the forked upstream's hash vote (spec §6.3,
	// row U18).
	fork *chainsim.Block
	// owed are, per pod, the §17 must-stay-0 series the scenario's own
	// probes and faults move by their spec row, by invariant or refusal
	// rule (owe); requireNoInvariantViolation expects exactly these.
	owed map[int]map[string]float64
}

// owe records that the scenario's own probe or fault moves series
// (an invariant or a refusal rule) by n on pod: each band probe of a block
// no vendor has is one retried_missing (§6.4, rows U12, X8, U13, K7), a
// rewind of every upstream one latest_regressed (§17, row C5).
func (s *scenario) owe(pod int, series string, n float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owed == nil {
		s.owed = map[int]map[string]float64{}
	}
	if s.owed[pod] == nil {
		s.owed[pod] = map[string]float64{}
	}
	s.owed[pod][series] += n
}

// owedOn is what the scenario owes pod for series.
func (s *scenario) owedOn(pod int, series string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owed[pod][series]
}

// minorityGrace reports whether an HTTP read on pod that started at start
// may answer the minority-fork block answer names.
func (s *scenario) minorityGrace(pod int, start time.Time, answer string) bool {
	s.mu.Lock()
	fork := s.fork
	s.mu.Unlock()
	b := s.blockOf(answer)
	if fork == nil || b == nil || !b.Minority() {
		return false
	}
	latest, _ := s.forkFirsts(pod)
	return latest.IsZero() || start.Before(latest.Add(liveBound+tapProcessingSlack))
}

// forkFirsts is when a minority-fork block first reached pod as a latest
// answer, and when a minority-fork header first reached it at all; zero
// before either.
func (s *scenario) forkFirsts(pod int) (latest, header time.Time) {
	first := func(byHash map[hash]time.Time) (t time.Time) {
		for bh, at := range byHash {
			if b := s.chain.Block(bh); b != nil && b.Minority() && (t.IsZero() || at.Before(t)) {
				t = at
			}
		}
		return t
	}
	s.h.headersMu.Lock()
	defer s.h.headersMu.Unlock()
	return first(s.h.latests[pod]), first(s.h.headers[pod])
}

// excuse: checks on pod (-1 any) during [from, to] may fail, because the
// scenario's fault makes the pod unable to serve; to zero is open.
type excuse struct {
	pod      int
	from, to time.Time
}

type reorg struct {
	at  time.Time
	old []*chainsim.Block
	tip *chainsim.Block
}

type memoEntry struct {
	done chan struct{}
	val  string
}

func newScenario(h *harness, sp spec, chainID int64) *scenario {
	if sp.project == "" {
		sp.project = "rw"
	}
	if sp.vendors == nil {
		sp.vendors = baseVendors
	}
	s := &scenario{spec: sp, h: h, vendors: map[string]*chainsim.Vendor{}, proxies: map[string]*proxy{},
		memo: map[string]*memoEntry{}, allowSet: map[string]bool{}, rpcs: map[string][]*rpc.Client{}}
	for _, a := range sp.allow {
		s.allowSet[a] = true
	}
	s.dir = filepath.Join(h.dir, sp.name)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	s.tr = newTranscript(s.dir, "scenario")
	if sp.sim.ChainID != 0 {
		chainID = sp.sim.ChainID
	}
	cfg := sp.sim
	cfg.ChainID, cfg.Seed = chainID, uint64(chainID)
	if cfg.BlockTime == 0 {
		cfg.BlockTime = blockTime
	}
	s.chain = chainsim.NewChain(cfg)
	s.net = &netSpec{project: sp.project, chainID: chainID, integrity: sp.integrity, integrityOff: sp.integrityOff, checks: sp.checks,
		minimal: sp.minimal, evm: sp.evm, extra: sp.network, cache: sp.cache}
	slug := strings.ToLower(strings.ReplaceAll(sp.name, "/", "-"))
	for _, vs := range sp.vendors {
		id := slug + "-" + vs.key
		v, err := s.chain.AddVendor(id, vs.name, vs.ws)
		if err != nil {
			h.t.Fatal(err)
		}
		px := h.newProxy(id, v.Addr(), vs.ws)
		s.vendors[vs.key], s.proxies[vs.key] = v, px
		if f, ok := sp.faults[vs.key]; ok {
			v.SetFaults(f)
		}
		s.net.ups = append(s.net.ups, upSpec{id: id, endpoint: px.URL(), vendor: vs.name, tags: sp.tags[vs.key], evm: sp.upEvm[vs.key],
			extra: sp.up[vs.key], budget: sp.budgets[vs.key]})
	}
	s.chain.Start()
	for _, key := range sp.late {
		s.vendors[key].Update(func(f *chainsim.Faults) { f.ChainIDHeld = true })
	}
	return s
}

// upstream is the upstream id of a vendor key.
func (s *scenario) upstream(key string) string {
	return strings.ToLower(strings.ReplaceAll(s.name, "/", "-")) + "-" + key
}

func (s *scenario) network() string { return fmt.Sprintf("evm:%d", s.chain.ID()) }

// run is one TestRealWorld run: its dir, the host load record, and one
// summary row per scenario.
type run struct {
	root string
	host *hostLoad

	mu   sync.Mutex
	rows []summaryRow
}

type summaryRow struct {
	phase, name, result string
	wall                time.Duration
	load                string
}

// phase boots fresh redis and pods for specs, then runs every spec
// concurrently on them, each on its own chain id from firstChain. Each
// scenario runs in its own goroutine rather than as a t.Parallel subtest,
// so -test.parallel never holds a scenario back while its chain runs.
func (r *run) phase(t *testing.T, name string, firstChain int64, specs []spec) {
	t.Run(name, func(t *testing.T) {
		h := newHarness(t, filepath.Join(r.root, name), !specs[0].memory)
		h.host = r.host
		h.vars = map[string]time.Duration{}
		for _, sp := range specs {
			for name, d := range sp.vars {
				if old, ok := h.vars[name]; ok && old != d {
					t.Fatalf("phase %s: %s sets %s to %s, another scenario to %s", name, sp.name, name, d, old)
				}
				h.vars[name] = d
			}
		}
		var scs []*scenario
		var nets []*netSpec
		for i, sp := range specs {
			sc := newScenario(h, sp, firstChain+int64(i))
			scs = append(scs, sc)
			nets = append(nets, sc.net)
		}
		pods := specs[0].pods
		if pods == 0 {
			pods = 2
		}
		h.webSocket = specs[0].webSocket
		h.startPods(pods, specs[0].oldPods, specs[0].cold, nets)
		var wg sync.WaitGroup
		for _, sc := range scs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t.Run(sc.name, func(t *testing.T) {
					start := time.Now()
					defer func() {
						result := "pass"
						if t.Failed() {
							result = "fail"
						}
						r.mu.Lock()
						r.rows = append(r.rows, summaryRow{phase: name, name: sc.name, result: result, wall: time.Since(start), load: r.host.record(start, time.Now())})
						r.mu.Unlock()
					}()
					sc.execute(t)
				})
			}()
		}
		wg.Wait()
	})
}

// summary prints one line per scenario, by phase and name: result, wall
// time, load record.
func (r *run) summary(t *testing.T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slices.SortFunc(r.rows, func(a, b summaryRow) int { return cmp.Or(cmp.Compare(a.phase, b.phase), cmp.Compare(a.name, b.name)) })
	var b strings.Builder
	b.WriteString("run summary:\n")
	for _, row := range r.rows {
		fmt.Fprintf(&b, "  %-16s %-8s %s %6s  %s\n", row.phase, row.name, row.result, row.wall.Round(time.Second), row.load)
	}
	t.Log(b.String())
}

func (s *scenario) execute(t *testing.T) {
	s.t = t
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.chain.Close()
	defer s.cancel()
	s.httpc = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}, Timeout: httpCheckWait}
	for _, p := range s.h.pods {
		if p.running() {
			s.waitServing(p.idx)
		}
	}
	for _, key := range s.late {
		s.vendors[key].Update(func(f *chainsim.Faults) { f.ChainIDHeld = false })
		s.logf("%s registers after the network serves on every pod", s.upstream(key))
	}
	start := time.Now()
	if s.clients != nil {
		s.clients(s)
	} else {
		s.baseClients()
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.watchStalls() }()
	s.body(s)
	s.finish()
	// §19: every scenario asserts the invariants on every new-image pod.
	for _, p := range s.h.pods {
		if p.running() && !p.old {
			s.requireNoInvariantViolation(p.idx)
		}
	}
	if s.after != nil {
		s.after(s)
	}
	s.judge(time.Since(start))
}

// stallDump: a header tap silent this long, or past its scenario's bound
// when that is longer, on a pod that should serve it gets the pod a SIGQUIT
// goroutine dump (triage, not a bound) while the host is below its load limit.
const stallDump = 10 * time.Second

// watchStalls dumps a pod's goroutines, once per pod and phase, when one of
// the scenario's header taps is silent past stallDump and the scenario's
// own bound (a scenario such as U11 waits for a bench by spec) while the chain
// mints and no fault excuses the pod; it then restarts the pod. Silence
// counts from the later of the tap's last block and the mint of the block
// after it, so a halted chain silences no tap. Verdicts of the phase's
// other scenarios on that pod after the dump carry the restart. On an
// overloaded host the pod keeps running: a host that pages the -race pods
// out stalls both at once, and restarting them turned that one stall into
// connection failures in every scenario of the phase; the tap's gap verdict
// still fails, labelled.
func (s *scenario) watchStalls() {
	after := max(stallDump, s.bound)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Second):
		}
		s.mu.Lock()
		var taps []*clientBase
		for _, c := range s.heads {
			taps = append(taps, &c.clientBase)
		}
		for _, g := range s.grpcs {
			taps = append(taps, &g.clientBase)
		}
		s.mu.Unlock()
		for _, c := range taps {
			pod, since, last, ok := c.silence()
			if ok && last > 0 {
				next := s.chain.CanonAt(last + 1)
				if next == nil {
					continue
				}
				since = min(since, time.Since(next.MintedAt))
			}
			if !ok || c.slow || since < after || s.excused(pod, time.Now().Add(-since), time.Now()) || !s.h.claimDump(pod) {
				continue
			}
			if load, over := s.h.host.overloaded(time.Now().Add(-since), time.Now()); over {
				s.logf("host overloaded (load %.1f): %s silent %s on pod-%d; no dump, pod-%d kept", load, c.name, since.Round(time.Second), pod+1, pod+1)
				continue
			}
			p := s.h.pods[pod]
			log := p.quit()
			s.logf("%s silent %s on pod-%d: SIGQUIT goroutine dump in %s; restarting pod-%d", c.name, since.Round(time.Second), pod+1, log, pod+1)
			if err := p.start(); err != nil {
				s.logf("pod-%d restart after the dump: %v", pod+1, err)
			}
		}
	}
}

// waitServing waits until pod idx serves the scenario network's head: 5
// blocks below the second-highest vendor view, as quorum 2 serves, and, on
// a new-image pod, a confirmed head, so the scenario's subscriptions start
// at the subscribe and their logs ranges are known (logsConn); a headless
// pod is asked that block by number.
func (s *scenario) waitServing(idx int) {
	want := s.vendorHead(1) - 5
	deadline := time.Now().Add(podBootWait)
	for {
		var got string
		var err error
		if s.idle {
			if s.confirmedHeadSeen(idx) {
				s.tr.logf("pod-%d has a confirmed head", idx+1)
				return
			}
			got = "no confirmed head"
		} else if s.isHeadless(idx) {
			got = s.blockAnswer(idx, "", "eth_getBlockByNumber", hexutil.EncodeUint64(want), false)
			if got == s.chain.CanonAt(want).Hash.Hex() {
				s.tr.logf("pod-%d serves %d by number", idx+1, want)
				return
			}
		} else {
			var bn hexutil.Uint64
			err = s.call(idx, &bn, "eth_blockNumber")
			if err == nil && uint64(bn) >= want {
				if s.h.pods[idx].old || s.confirmedHeadSeen(idx) {
					s.tr.logf("pod-%d serves %d", idx+1, bn)
					return
				}
				got = fmt.Sprintf("%d and no confirmed head", uint64(bn))
			} else {
				got = fmt.Sprint(uint64(bn))
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("pod-%d does not serve %s: wants %d, got %s, err %v", idx+1, s.network(), want, got, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// vendorHead is the (rank+1)-th highest head the vendors serve.
func (s *scenario) vendorHead(rank int) uint64 {
	var hs []uint64
	for _, v := range s.vendors {
		hs = append(hs, v.Head().Number)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i] > hs[j] })
	return hs[min(rank, len(hs)-1)]
}

func (s *scenario) isNumbered(pod int) bool { return slices.Contains(s.numbered, pod) }

func (s *scenario) isHeadless(pod int) bool {
	return s.isNumbered(pod) || slices.Contains(s.headless, pod)
}

// client is the HTTP client of pod for use-upstream selector sel ("" none).
func (s *scenario) client(pod int, sel string) *rpc.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.rpcs[sel]
	if cs == nil {
		for _, p := range s.h.pods {
			opts := []rpc.ClientOption{rpc.WithHTTPClient(s.httpc)}
			if sel != "" {
				opts = append(opts, rpc.WithHeader("X-ERPC-Use-Upstream", sel))
			}
			c, err := rpc.DialOptions(context.Background(), p.httpURL(s.project, s.chain.ID()), opts...)
			if err != nil {
				panic(err) // an HTTP URL only fails to parse
			}
			cs = append(cs, c)
		}
		s.rpcs[sel] = cs
	}
	return cs[pod]
}

func (s *scenario) call(pod int, result interface{}, method string, args ...interface{}) error {
	return s.callAs(pod, "", result, method, args...)
}

// callAs calls method on pod through use-upstream selector sel. eRPC's own
// rate-limit refusal (-32005, over HTTP status 429 too) says nothing about
// the chain, so the call is tried again until httpCheckWait (D8 spends
// every upstream budget; U5 throttles every vendor).
func (s *scenario) callAs(pod int, sel string, result interface{}, method string, args ...interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), httpCheckWait)
	defer cancel()
	for {
		err := s.client(pod, sel).CallContext(ctx, result, method, args...)
		var re rpc.Error
		var he rpc.HTTPError
		limited := errors.As(err, &re) && re.ErrorCode() == rateLimitedCode ||
			errors.As(err, &he) && he.StatusCode == http.StatusTooManyRequests
		if !limited || ctx.Err() != nil {
			return err
		}
		sleepCtx(ctx, rateLimitedRetry)
	}
}

// rateLimitedCode is the JSON-RPC code of a rate-limit refusal;
// rateLimitedRetry, harness: the wait before trying again.
const (
	rateLimitedCode  = -32005
	rateLimitedRetry = 50 * time.Millisecond
)

//
// --- clients ---
//

// initBase fills a client's shared part; slow clients stop reading on
// purpose, and allow names the ends and jumps the client alone may see.
func (s *scenario) initBase(c *clientBase, name string, pod int, slow bool, allow []string) {
	c.s, c.name, c.pod, c.slow = s, name, pod, slow
	c.tr = newTranscript(s.dir, name)
	c.reconnect = make(chan struct{}, 1)
	c.allow = map[string]bool{}
	for _, a := range allow {
		c.allow[a] = true
	}
}

// addHeads opens a ws newHeads client; a slow one reads a raw WebSocket
// with a small receive buffer, so pausing it backs up into eRPC.
func (s *scenario) addHeads(name string, pod int, slow bool, allow ...string) *headsClient {
	c := &headsClient{}
	s.initBase(&c.clientBase, name, pod, slow, allow)
	s.mu.Lock()
	s.heads = append(s.heads, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); c.run(s.ctx) }()
	return c
}

// addHeadsOn opens a ws newHeads client on use-upstream lane sel ("" the
// network-wide one); a pinned one redials its own pod after an end.
func (s *scenario) addHeadsOn(name string, pod int, sel string, pinned bool, allow ...string) *headsClient {
	c := &headsClient{}
	s.initBase(&c.clientBase, name, pod, false, allow)
	c.selector, c.pinned = sel, pinned
	s.mu.Lock()
	s.heads = append(s.heads, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); c.run(s.ctx) }()
	return c
}

func (s *scenario) addLogs(name string, pod, filter int) *logsClient {
	c := &logsClient{filter: filter, live: map[chainsim.LogKey]liveLog{}, ever: map[chainsim.LogKey]bool{}}
	s.initBase(&c.clientBase, name, pod, false, nil)
	s.mu.Lock()
	s.logs = append(s.logs, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); c.run(s.ctx) }()
	return c
}

func (s *scenario) addGrpc(name string, pod, switchEvery int, slow bool, allow ...string) *grpcClient {
	c := &grpcClient{switchEvery: switchEvery}
	s.initBase(&c.clientBase, name, pod, slow, allow)
	s.mu.Lock()
	s.grpcs = append(s.grpcs, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); c.run(s.ctx) }()
	return c
}

// grpcSwitchBlocks: baseline gRPC clients resume on the other pod every
// 15–30 blocks.
const grpcSwitchBlocks = 15

// baseClients are baseline B's clients on each pod: 2 newHeads, 2 logs
// (one per filter), 2 gRPC streams that resume on the other pod.
func (s *scenario) baseClients() {
	for _, p := range s.h.pods {
		s.podClients(p.idx, grpcSwitchBlocks)
	}
}

// podClients are baseline B's clients of one pod only; its gRPC streams
// resume on another pod every switchEvery blocks (0 never).
func (s *scenario) podClients(pod, switchEvery int) {
	for i := range 2 {
		s.addHeads(fmt.Sprintf("p%d-heads-%d", pod+1, i+1), pod, false)
		s.addLogs(fmt.Sprintf("p%d-logs-%d", pod+1, i+1), pod, i)
		s.addGrpc(fmt.Sprintf("p%d-grpc-%d", pod+1, i+1), pod, switchEvery, false)
	}
}

//
// --- faults and time ---
//

func (s *scenario) blocks(n int) { time.Sleep(time.Duration(n) * s.chain.BlockTime()) }

func (s *scenario) logf(format string, args ...interface{}) {
	s.tr.logf(format, args...)
	s.t.Logf(format, args...)
}

// reorg replaces the top depth canonical blocks.
func (s *scenario) reorg(depth int) reorg {
	old, tip := s.chain.Reorg(depth)
	r := reorg{at: time.Now(), old: old, tip: tip}
	s.mu.Lock()
	s.reorgs = append(s.reorgs, r)
	s.mu.Unlock()
	s.logf("reorg depth %d: fork point %d, new head %d %s", depth, old[0].Number-1, tip.Number, short(tip.Hash))
	return r
}

// rewind drops the top depth canonical blocks on every vendor; the chain
// goes on from the lower head. Clients that received a dropped block learn
// of it as of a reorg.
func (s *scenario) rewind(depth int) reorg {
	old, tip := s.chain.Rewind(depth)
	r := reorg{at: time.Now(), old: old, tip: tip}
	s.mu.Lock()
	s.reorgs = append(s.reorgs, r)
	s.mu.Unlock()
	s.logf("rewind depth %d: new head %d %s", depth, tip.Number, short(tip.Hash))
	return r
}

// upSpec is the upstream of a vendor key as the pods' config declares it.
func (s *scenario) upSpec(key string) upSpec {
	id := s.upstream(key)
	for _, u := range s.net.ups {
		if u.id == id {
			return u
		}
	}
	s.t.Fatalf("%s has no upstream %s", s.name, id)
	return upSpec{}
}

func (s *scenario) excuse(pod int, from time.Time) *excuse {
	e := &excuse{pod: pod, from: from}
	s.mu.Lock()
	s.excuses = append(s.excuses, e)
	s.mu.Unlock()
	return e
}

func (s *scenario) endExcuse(e *excuse, to time.Time) {
	s.mu.Lock()
	e.to = to
	s.mu.Unlock()
}

// excused reports whether pod had an excuse overlapping [from, to].
func (s *scenario) excused(pod int, from, to time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.excuses {
		if e.pod >= 0 && e.pod != pod {
			continue
		}
		if !to.Before(e.from) && (e.to.IsZero() || !from.After(e.to)) {
			return true
		}
	}
	return false
}

func (s *scenario) allowed(key string) bool { return s.allowSet[key] }

func (s *scenario) violate(client, format string, args ...interface{}) {
	msg := fmt.Sprintf("%s [%s] %s", time.Now().Format("15:04:05.000"), client, fmt.Sprintf(format, args...))
	s.mu.Lock()
	s.violations = append(s.violations, msg)
	s.mu.Unlock()
	s.tr.logf("VIOLATION %s", msg)
}

// metric sums a metric's samples on pod for this scenario's project and
// network, plus labels.
func (s *scenario) metric(pod int, name string, labels map[string]string) (float64, bool) {
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		return 0, false
	}
	match := map[string]string{"project": s.project, "network": s.network()}
	for k, v := range labels {
		match[k] = v
	}
	return sumOf(ss, name, match)
}

// tapsLost sums pod's tap ends for this scenario that a client sees as a
// restart or a close: out of range or lagging, not its own unsubscribe.
func (s *scenario) tapsLost(pod int) float64 {
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		return 0
	}
	var n float64
	for _, reason := range []string{"out_of_range", "lagging"} {
		v, _ := sumOf(ss, "erpc_tap_end_total", map[string]string{"project": s.project, "network": s.network(), "reason": reason})
		n += v
	}
	return n
}

// confirmedHeadSeen reports whether pod's network-wide tracker has had a
// confirmed head (spec §17: the gauge is set on each confirmed-head change),
// false when the scrape fails. A tracker keeps its pins once it has one
// (spec §9 floor), so a tap opened after it starts at or below the next
// block.
func (s *scenario) confirmedHeadSeen(pod int) bool {
	v, ok := s.metric(pod, "erpc_network_subscription_last_head_timestamp_seconds", nil)
	return ok && v > 0
}

// waitMetric scrapes pod until the metric exists and cond holds, or within
// passes; it returns the last value and whether cond held.
func (s *scenario) waitMetric(pod int, name string, labels map[string]string, within time.Duration, cond func(float64) bool) (float64, bool) {
	deadline := time.Now().Add(within)
	for {
		v, found := s.metric(pod, name, labels)
		if found && cond(v) {
			return v, true
		}
		if time.Now().After(deadline) {
			return v, false
		}
		time.Sleep(scrapeEvery)
	}
}

//
// --- oracle ---
//

func (s *scenario) memoize(key string, fn func() string) string {
	s.mu.Lock()
	e := s.memo[key]
	if e == nil {
		e = &memoEntry{done: make(chan struct{})}
		s.memo[key] = e
		s.mu.Unlock()
		e.val = fn()
		close(e.done)
		return e.val
	}
	s.mu.Unlock()
	<-e.done
	return e.val
}

// oracleMemo memoizes an oracle answer from target per excuse state: an
// answer taken while target was excused (cut, killed) says nothing about
// it after the excuse, so a check that starts later asks again (N4: pod-2
// delivers a block while pod-1 is cut; pod-1 delivers it after the heal).
func (s *scenario) oracleMemo(target int, key string, fn func() string) string {
	now := time.Now()
	return s.memoize(fmt.Sprintf("%s/excused-%t", key, s.excused(target, now, now)), fn)
}

// first reports whether this is the first call with key.
func (s *scenario) first(key string) bool {
	ran := false
	s.memoize(key, func() string { ran = true; return "" })
	return ran
}

// blockAnswer is a block answer's hash, "null", or the error.
func (s *scenario) blockAnswer(pod int, sel string, method string, args ...interface{}) string {
	var raw json.RawMessage
	err := s.callAs(pod, sel, &raw, method, args...)
	return answerHash(raw, err)
}

// answerHash is a block answer's hash, "null", or the error.
func answerHash(raw json.RawMessage, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "null"
	}
	var h struct {
		Hash hash `json:"hash"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "unparsable: " + err.Error()
	}
	return h.Hash.Hex()
}

// checkHead applies the oracle to a newHeads delivery on pod, through the
// client's use-upstream selector: the same pod's eth_blockNumber covers it
// at once; by hash it is on every pod; by number it is on the same pod at
// once and on another within otherInstanceWait, or a later reorg tells the
// client otherwise. Numbered pods get the by-number check only.
func (s *scenario) checkHead(c *headsClient, pod int, n uint64, h hash, delivered time.Time) {
	s.checks.Add(1)
	go func() {
		defer s.checks.Done()
		start := time.Now()
		sel := c.selector
		if !s.isHeadless(pod) {
			var bn hexutil.Uint64
			err := s.callAs(pod, sel, &bn, "eth_blockNumber")
			if (err != nil || uint64(bn) < n) && !s.excused(pod, start, time.Now()) {
				s.violate(c.name, "pod-%d: eth_blockNumber %d (err %v) below the delivered head %d", pod+1, bn, err, n)
			}
		}
		for target := range s.h.pods {
			if !s.isNumbered(target) {
				got := s.oracleMemo(target, fmt.Sprintf("hash/%d/%s/%s/from-%d", target, sel, h.Hex(), pod), func() string {
					return s.blockAnswer(target, sel, "eth_getBlockByHash", h.Hex(), false)
				})
				if got != h.Hex() && !s.excused(target, start, time.Now()) {
					s.violate(c.name, "pod-%d: eth_getBlockByHash(%s) delivered on pod-%d answered %s", target+1, short(h), pod+1, got)
				}
			}
			read := time.Now()
			got := s.oracleMemo(target, fmt.Sprintf("number/%d/%s/%d/%s/from-%d", target, sel, n, h.Hex(), pod), func() string {
				got := s.blockAnswer(target, sel, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
				if got != h.Hex() && target != pod {
					// Another instance may still be on the branch the
					// delivering one left, until it has followed the same
					// voters.
					time.Sleep(time.Until(delivered.Add(otherInstanceWait)))
					got = s.blockAnswer(target, sel, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
				}
				return got
			})
			if got != h.Hex() && !s.excused(target, start, time.Now()) && !s.minorityGrace(target, read, got) {
				c.mismatch(pendingNumber{target: target, number: n, hash: h, got: got, delivered: delivered})
			}
		}
	}()
}

// checkLog applies the oracle to a delivered log: eth_getLogs by its block
// hash returns it on every pod; on a numbered pod, by its block number.
func (s *scenario) checkLog(c *logsClient, pod int, l chainsim.Log) {
	s.checks.Add(1)
	go func() {
		defer s.checks.Done()
		start := time.Now()
		for target := range s.h.pods {
			by := "blockHash"
			if s.isNumbered(target) {
				by = "number"
			}
			got := s.oracleMemo(target, fmt.Sprintf("logs/%d/%s/%d/from-%d", target, l.BlockHash.Hex(), c.filter, pod), func() string {
				q := chainsim.Filters[c.filter].Params()
				if by == "number" {
					q["fromBlock"] = hexutil.EncodeUint64(l.BlockNumber)
					q["toBlock"] = hexutil.EncodeUint64(l.BlockNumber)
				} else {
					q["blockHash"] = l.BlockHash.Hex()
				}
				var logs []logJSON
				if err := s.call(target, &logs, "eth_getLogs", q); err != nil {
					return "error: " + err.Error()
				}
				var b strings.Builder
				b.WriteString(",")
				for _, x := range logs {
					if x.BlockHash == l.BlockHash {
						fmt.Fprintf(&b, "%d,", x.LogIndex)
					}
				}
				return b.String()
			})
			if !strings.Contains(got, fmt.Sprintf(",%d,", l.LogIndex)) && !s.excused(target, start, time.Now()) {
				s.violate(c.name, "pod-%d: eth_getLogs by %s %s (block %d) misses log %d delivered on pod-%d: %s", target+1, by, short(l.BlockHash), l.BlockNumber, l.LogIndex, pod+1, got)
			}
		}
	}()
}

//
// --- end of run ---
//

// finish halts the chain, waits until every client holds its lane's final
// head, then stops the clients and the pending checks.
func (s *scenario) finish() {
	s.chain.Halt(true)
	s.mu.Lock()
	s.haltAt = time.Now()
	heads, grpcs, logs := slices.Clone(s.heads), slices.Clone(s.grpcs), slices.Clone(s.logs)
	s.mu.Unlock()
	tip := s.chain.Tip()
	s.logf("halted at %d %s", tip.Number, short(tip.Hash))
	deadline := time.Now().Add(finalSettle)
	await := func(cb *clientBase, hc *headerChain) {
		final := s.finalHead(cb.selector)
		has := func() bool {
			cb.mu.Lock()
			defer cb.mu.Unlock()
			return hc.m[final.Number] == final.Hash
		}
		for !has() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if !has() {
			s.violate(cb.name, "never received the final head %d %s within %s of the halt", final.Number, short(final.Hash), finalSettle)
		}
	}
	for _, c := range heads {
		await(&c.clientBase, &c.hc)
	}
	for _, g := range grpcs {
		await(&g.clientBase, &g.hc)
	}
	// A logs subscription behind the final head is owed its logs or, once
	// its tap ended, a 1013 within logsCloseSpread (§9.2); judging it
	// earlier counts logs the server may still close over as missing.
	logsDeadline := time.Now().Add(logsCloseSpread + reconnectSlack)
	for _, c := range logs {
		f := chainsim.Filters[c.filter]
		last := uint64(0)
		for n := s.finalHead(c.selector).Number; n > 0 && last == 0; n-- {
			if len(s.chain.CanonicalLogs(f, n, n)) > 0 {
				last = n
			}
		}
		owed := func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, lc := range c.conns {
				if lc.open && lc.from > 0 && lc.from <= last && lc.highest < last {
					return true
				}
			}
			return false
		}
		for owed() && time.Now().Before(logsDeadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Logs follow one receipts fetch after their head (§10).
	time.Sleep(3 * liveBound)
	s.cancel()
	s.wg.Wait()
	s.checks.Wait()
}

// finalHead is the last head a client on use-upstream selector sel can get
// after the halt: its lane's Latest, the q-th highest head of the vendors
// behind the upstreams its selector matches; the network-wide lane ("")
// matches every vendor (§6.4; §6.5: no block above Latest is sent), so a
// lane whose vendors lag stops below the chain's tip.
func (s *scenario) finalHead(sel string) *chainsim.Block {
	var heads []*chainsim.Block
	for key, v := range s.vendors {
		ok := sel == ""
		if !ok {
			var err error
			if ok, err = common.MatchesSelector(sel, s.upstream(key), s.tags[key]); err != nil {
				s.t.Fatalf("selector %q of %s: %v", sel, s.name, err)
			}
		}
		if h := v.Head(); ok && h != nil {
			heads = append(heads, h)
		}
	}
	if len(heads) == 0 {
		s.t.Fatalf("selector %q matches no vendor of %s", sel, s.name)
	}
	slices.SortFunc(heads, func(a, b *chainsim.Block) int { return cmp.Compare(b.Number, a.Number) })
	return heads[s.servedTipQuorum().Of(len(heads))-1]
}

// servedTipQuorum is the scenario network's servedTip.quorum, parsed as
// eRPC parses it.
func (s *scenario) servedTipQuorum() common.ServedTipQuorum {
	var evm struct {
		ServedTip *common.EvmServedTipConfig `json:"servedTip"`
	}
	raw, err := json.Marshal(s.net.evm)
	if err == nil {
		err = json.Unmarshal(raw, &evm)
	}
	if err != nil {
		s.t.Fatalf("servedTip of %s: %v", s.name, err)
	}
	if evm.ServedTip == nil {
		return 0
	}
	return evm.ServedTip.Quorum
}

// judge applies the end-of-run oracle and fails the scenario with its
// violations.
func (s *scenario) judge(wall time.Duration) {
	var lat []time.Duration
	for _, c := range s.heads {
		s.judgeChain(c.name, &c.hc, s.finalHead(c.selector))
		for _, p := range c.unresolved() {
			s.violate(c.name, "pod-%d: eth_getBlockByNumber(%d) answered %s, not the delivered %s, and no reorg at or below %d followed", p.target+1, p.number, p.got, short(p.hash), p.number)
		}
		lat = append(lat, s.judgeDelivery(&c.clientBase, true)...)
		c.tr.close()
	}
	for _, g := range s.grpcs {
		s.judgeChain(g.name, &g.hc, s.finalHead(g.selector))
		lat = append(lat, s.judgeDelivery(&g.clientBase, true)...)
		g.tr.close()
	}
	for _, c := range s.logs {
		s.judgeLogs(c)
		lat = append(lat, s.judgeDelivery(&c.clientBase, false)...)
		c.tr.close()
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	if len(lat) > 0 {
		s.logf("%d deliveries in %s: latency p50 %s p99 %s max %s (bound %s)", len(lat), wall.Round(time.Second),
			lat[len(lat)/2].Round(time.Millisecond), lat[len(lat)*99/100].Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond), s.bound)
	}
	for _, key := range s.late {
		up := s.upstream(key)
		for pod := range s.h.pods {
			heads := func(outcome string) float64 {
				v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": up, "outcome": outcome})
				return v
			}
			if v := heads("voted"); v == 0 {
				s.violate(s.name, "pod-%d: %s, registered after the network started, never voted", pod+1, up)
			}
			if v := heads("gated_voter"); v > 0 {
				s.violate(s.name, "pod-%d: %v heads of %s, registered after the network started, counted gated_voter", pod+1, v, up)
			}
		}
	}
	s.mu.Lock()
	vs := slices.Clone(s.violations)
	s.mu.Unlock()
	s.tr.close()
	if len(vs) == 0 {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, "violations.txt"), []byte(strings.Join(vs, "\n")+"\n"), 0o644)
	shown := vs[:min(len(vs), 40)]
	s.t.Errorf("%s: %d violations (all in %s):\n%s", s.name, len(vs), filepath.Join(s.dir, "violations.txt"), strings.Join(shown, "\n"))
}

// judgeChain: the client's chain, from its last segment start to the final
// head, is the canonical chain.
func (s *scenario) judgeChain(name string, hc *headerChain, tip *chainsim.Block) {
	if !hc.started {
		s.violate(name, "received no header")
		return
	}
	for n := hc.segStart; n <= tip.Number; n++ {
		want := s.chain.CanonAt(n).Hash
		if got, ok := hc.m[n]; !ok || got != want {
			s.violate(name, "final chain at %d is %s, canonical %s", n, short(got), short(want))
		}
	}
}

// judgeDelivery checks each live delivery against the scenario's bound,
// counted from when the block's first header reached the pod (waitFrom):
// to its receipt, and for header streams the wait between receipts. A
// block minted before the client's connection opened is catch-up, not
// live delivery.
func (s *scenario) judgeDelivery(c *clientBase, stalls bool) []time.Duration {
	c.mu.Lock()
	ds := slices.Clone(c.deliveries)
	c.mu.Unlock()
	if c.slow {
		return nil
	}
	var out []time.Duration
	for _, d := range ds {
		if d.minted.Before(d.connStart) {
			continue
		}
		from := s.waitFrom(d.pod, d.hash, d.minted)
		l := d.at.Sub(from)
		out = append(out, l)
		if l > s.bound && !s.excused(d.pod, from, d.at) {
			msg := fmt.Sprintf("pod-%d: block %d %s reached the client %s after its first header there, %s after it was minted (bound %s)",
				d.pod+1, d.number, short(d.hash), l.Round(time.Millisecond), d.at.Sub(d.minted).Round(time.Millisecond), s.bound)
			s.violate(c.name, "%s", s.h.host.verdict(from, d.at, msg))
		}
	}
	if !stalls {
		return out
	}
	for i := 1; i <= len(ds); i++ {
		var from, to time.Time
		var pod int
		switch {
		case i == len(ds):
			from, to, pod = ds[i-1].at, s.haltAt, ds[i-1].pod
		case ds[i].connStart != ds[i-1].connStart:
			continue
		default:
			from, to, pod = ds[i-1].at, ds[i].at, ds[i].pod
		}
		if to.After(s.haltAt) {
			to = s.haltAt
		}
		// The tap waits from the later of its last block and the first
		// header of the block after it.
		next := s.chain.CanonAt(ds[i-1].number + 1)
		if next == nil {
			continue
		}
		if at := s.waitFrom(pod, next.Hash, next.MintedAt); at.After(from) {
			from = at
		}
		if gap := to.Sub(from); gap > s.bound && !s.excused(pod, from, to) {
			msg := fmt.Sprintf("pod-%d: no block from %s to %s (%s, bound %s)", pod+1, from.Format("15:04:05.000"), to.Format("15:04:05.000"), gap.Round(time.Millisecond), s.bound)
			s.violate(c.name, "%s", s.h.host.verdict(from, to, msg))
		}
	}
	return out
}

// waitFrom is where a delivery's bound starts: when the block's first
// header from any upstream reached the pod (spec §15.1, rows U3 and X9),
// or its mint while none was seen there.
func (s *scenario) waitFrom(pod int, bh hash, minted time.Time) time.Time {
	if at, ok := s.h.headerAt(pod, bh); ok && at.After(minted) {
		return at
	}
	return minted
}

// judgeLogs: over each subscription's covered range, the client's live
// logs are the canonical logs its filter matches.
func (s *scenario) judgeLogs(c *logsClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := chainsim.Filters[c.filter]
	covered := map[uint64]bool{}
	for _, lc := range c.conns {
		for n := lc.from; n <= lc.to && lc.to > 0; n++ {
			covered[n] = true
		}
	}
	if len(covered) == 0 {
		// A subscription the server ended covers nothing, and one opened
		// after the halt has nothing left to cover: with only those, the
		// client could not be judged (N4: a pod-1 tap ends late in its
		// catch-up after the heal; its 1013 comes after the halt).
		for _, lc := range c.conns {
			if !lc.serverEnd && !lc.opened.After(s.haltAt) {
				s.violate(c.name, "covered no block")
				return
			}
		}
		s.logf("%s covered no block: each subscription ended by the server or opened after the halt", c.name)
		return
	}
	want := map[chainsim.LogKey]bool{}
	for n := range covered {
		for k := range s.chain.CanonicalLogs(f, n, n) {
			want[k] = true
		}
	}
	missing, extra := 0, 0
	var sample []string
	for k := range want {
		if _, ok := c.live[k]; !ok {
			missing++
			if len(sample) < 5 {
				sample = append(sample, fmt.Sprintf("missing %s/%d", short(k.BlockHash), k.LogIndex))
			}
		}
	}
	for k, l := range c.live {
		if covered[l.number] && !want[k] {
			extra++
			if len(sample) < 10 {
				sample = append(sample, fmt.Sprintf("extra %d %s/%d", l.number, short(k.BlockHash), k.LogIndex))
			}
		}
	}
	if missing+extra > 0 {
		s.violate(c.name, "live logs over %d covered blocks differ from canonical: %d missing, %d not canonical: %s", len(covered), missing, extra, strings.Join(sample, ", "))
	}
}
