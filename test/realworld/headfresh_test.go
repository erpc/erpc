package realworld

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/erpc/erpc/test/chainsim"
)

// freshUp is one upstream of the freshness layout: an own reader polled
// fast, then vendors polled slowly, all https.
type freshUp struct {
	key, vendor string
	reader      bool
	slow        time.Duration
}

var freshUps = []freshUp{
	{"http-r", "reader", true, 2 * time.Millisecond},
	{"http-a", "vendor-a", false, 20 * time.Millisecond},
	{"http-b", "vendor-b", false, 30 * time.Millisecond},
	{"http-c", "vendor-c", false, 25 * time.Millisecond},
	{"http-d", "vendor-d", false, 40 * time.Millisecond},
}

// freshSpec is the freshness layout on Ethereum on Kubernetes' times
// divided by scale: 12 s blocks, the own reader polled every 1 s with a
// 100 ms debounce, vendors every 10 s; no ws source, quorum 2.
func freshSpec(name string, scale int, sim chainsim.Config, faults chainsim.Faults, body func(*scenario)) spec {
	d := func(t time.Duration) string { return (t / time.Duration(scale)).String() }
	var vendors []vendorSpec
	fs := map[string]chainsim.Faults{}
	upEvm := map[string]map[string]interface{}{}
	for _, u := range freshUps {
		vendors = append(vendors, vendorSpec{u.key, u.vendor, false})
		f := faults
		f.Slow = u.slow
		fs[u.key] = f
		if u.reader {
			upEvm[u.key] = map[string]interface{}{"statePollerInterval": d(time.Second), "statePollerDebounce": d(100 * time.Millisecond)}
		} else {
			upEvm[u.key] = map[string]interface{}{"statePollerInterval": d(10 * time.Second)}
		}
	}
	sim.BlockTime = 12 * time.Second / time.Duration(scale)
	return spec{name: name, pods: 2, oldPods: []int{0}, memory: true, bound: healthyDelivery,
		sim: sim, vendors: vendors, faults: fs, upEvm: upEvm, clients: func(*scenario) {}, body: body}
}

const (
	// p16Scale: 1.5 s blocks, reader 125 ms / 12.5 ms, vendors 1.25 s.
	p16Scale = 8
	// p16Rate: each image's client sends this many eth_blockNumber a second.
	p16Rate = 40
	// p16Run: about 400 blocks. Each pod polls the vendors at its own
	// phase, so the per-block delay spreads over a vendor interval: at 3
	// min the new-minus-old p50 moved by ±250 ms of the 3 s block from run
	// to run.
	p16Run = 10 * time.Minute
	// p16P50Slack, p16P90Slack: how much later the new image may first
	// answer a block (0.5 s and 1 s on Ethereum).
	p16P50Slack = 60 * time.Millisecond
	p16P90Slack = 120 * time.Millisecond
	// freshCostSlack: the new image may read the tag from its upstreams
	// at most this many times as often as the old image.
	freshCostSlack = 1.10
)

// P16: a poll-only Ethereum-shaped chain, time-scaled by p16Scale, quorum
// 2; pod 1 runs the old image, pod 2 the branch's, one eth_blockNumber
// client each. Per block, the time from chainsim minting it to each pod's
// eth_blockNumber first answering it: the new image is no later than the
// old, at no more latest reads.
func p16() spec {
	return freshSpec("P16", p16Scale, chainsim.Config{}, chainsim.Faults{}, p16Body)
}

// freshImage is one image's tally: every answer and when it arrived.
type freshImage struct {
	pod     int
	mu      sync.Mutex
	answers []freshFirst
	errs    int
}

// freshFirst is a value and a time: an answer, or a chain event.
type freshFirst struct {
	n  uint64
	at time.Time
}

func (im *freshImage) saw(n uint64, at time.Time) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.answers = append(im.answers, freshFirst{n, at})
}

// firstAt is when im first answered n or higher; false when never.
func (im *freshImage) firstAt(n uint64) (first time.Time, ok bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	for _, a := range im.answers {
		if a.n >= n && (!ok || a.at.Before(first)) {
			first, ok = a.at, true
		}
	}
	return first, ok
}

// freshClient calls read on im's pod every period until ctx ends.
func (s *scenario) freshClient(ctx context.Context, im *freshImage, period time.Duration, read func(pod int) (uint64, error)) {
	var wg sync.WaitGroup
	defer wg.Wait()
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := read(im.pod)
			at := time.Now()
			if err != nil {
				im.mu.Lock()
				im.errs++
				im.mu.Unlock()
				return
			}
			im.saw(n, at)
		}()
	}
}

// freshCount is one pod's upstream traffic per upstream key: its poller's
// polls of the tag (erpc_upstream_<tag>_block_polled_total), every
// eth_getBlockByNumber(tag) it sent (polls and forwarded client reads
// alike), and every request it sent.
type freshCount struct {
	polls, reads, numbered, requests map[string]float64
}

func (s *scenario) freshCounts(pod int, tag string) freshCount {
	c := freshCount{map[string]float64{}, map[string]float64{}, map[string]float64{}, map[string]float64{}}
	ss, err := s.h.pods[pod].scrape()
	if err != nil {
		s.t.Fatalf("scrape pod-%d: %v", pod+1, err)
	}
	for _, u := range freshUps {
		px, p := s.proxies[u.key], s.h.pods[pod]
		c.polls[u.key], _ = sumOf(ss, "erpc_upstream_"+tag+"_block_polled_total", map[string]string{"upstream": s.upstream(u.key)})
		c.reads[u.key] = float64(px.reads(p, tag))
		c.numbered[u.key] = float64(px.reads(p, "number"))
		c.requests[u.key] = float64(px.reads(p, anyRead))
	}
	return c
}

func (c freshCount) since(before freshCount) freshCount {
	for k := range c.polls {
		c.polls[k] -= before.polls[k]
		c.reads[k] -= before.reads[k]
		c.numbered[k] -= before.numbered[k]
		c.requests[k] -= before.requests[k]
	}
	return c
}

// freshRun runs one client per pod for run and returns each image's tally
// and its upstream traffic over the run.
func (s *scenario) freshRun(run, period time.Duration, tag string, read func(pod int) (uint64, error)) (imgs []*freshImage, counts []freshCount) {
	ctx, cancel := context.WithTimeout(s.ctx, run)
	defer cancel()
	var before []freshCount
	for _, p := range s.h.pods {
		imgs = append(imgs, &freshImage{pod: p.idx})
		before = append(before, s.freshCounts(p.idx, tag))
	}
	var wg sync.WaitGroup
	for _, im := range imgs {
		wg.Add(1)
		go func() { defer wg.Done(); s.freshClient(ctx, im, period, read) }()
	}
	wg.Wait()
	for i, p := range s.h.pods {
		counts = append(counts, s.freshCounts(p.idx, tag).since(before[i]))
	}
	return imgs, counts
}

// freshDelays are im's delays for each (value, since) event: from since
// to its first answer of value or higher; an event never answered counts
// from since to end.
func freshDelays(im *freshImage, events []freshFirst, end time.Time) (ds []time.Duration, missed int) {
	for _, e := range events {
		at, ok := im.firstAt(e.n)
		if !ok {
			at, missed = end, missed+1
		}
		ds = append(ds, max(at.Sub(e.at), 0))
	}
	slices.Sort(ds)
	return ds, missed
}

// freshVerdict is one image's result: its delays' p50 and p90; its polls,
// tag reads and upstream requests per unit, summed over upstreams; and its
// eth_getBlockByNumber(<number>) upstream calls per client read.
type freshVerdict struct {
	p50, p90               time.Duration
	polls, reads, requests float64
	numberedPerRead        float64
}

// freshReport writes each image's delays and traffic per unit to b and
// returns each image's verdict.
func (s *scenario) freshReport(b *strings.Builder, what, tag string, imgs []*freshImage, counts []freshCount, events []freshFirst, end time.Time, unit, run time.Duration) map[string]freshVerdict {
	out := map[string]freshVerdict{}
	per := float64(run) / float64(unit)
	for i, im := range imgs {
		img := s.image(im.pod)
		ds, missed := freshDelays(im, events, end)
		v := freshVerdict{p50: pctl(ds, 50), p90: pctl(ds, 90)}
		fmt.Fprintf(b, "pod-%d (%s image): %d %s, %d never answered, %d errors; delay p50 %s p90 %s max %s\n",
			im.pod+1, img, len(ds), what, missed, im.errs, v.p50, v.p90, pctl(ds, 100))
		fmt.Fprintf(b, "  per %s, %s polls / %s reads / all requests:", unit, tag, tag)
		var numbered float64
		for _, u := range freshUps {
			p, t, r := counts[i].polls[u.key]/per, counts[i].reads[u.key]/per, counts[i].requests[u.key]/per
			v.polls += p
			v.reads += t
			v.requests += r
			numbered += counts[i].numbered[u.key]
			fmt.Fprintf(b, " %s %.2f/%.2f/%.2f", u.vendor, p, t, r)
		}
		im.mu.Lock()
		clientReads := len(im.answers)
		im.mu.Unlock()
		if clientReads > 0 {
			v.numberedPerRead = numbered / float64(clientReads)
		}
		fmt.Fprintf(b, " (total %.2f/%.2f/%.2f); %.0f by-number block reads for %d client reads (%.3f each)\n",
			v.polls, v.reads, v.requests, numbered, clientReads, v.numberedPerRead)
		out[img] = v
	}
	return out
}

func p16Body(s *scenario) {
	s.blocks(10)
	start := time.Now()
	imgs, counts := s.freshRun(p16Run, time.Second/p16Rate, "latest", func(pod int) (uint64, error) {
		var bn hexutil.Uint64
		err := s.call(pod, &bn, "eth_blockNumber")
		return uint64(bn), err
	})
	end := time.Now()
	var events []freshFirst
	for _, b := range s.chain.MintedBetween(start.Add(s.chain.BlockTime()), end.Add(-s.chain.BlockTime())) {
		events = append(events, freshFirst{b.Number, b.MintedAt})
	}
	var b strings.Builder
	v := s.freshReport(&b, "blocks", "latest", imgs, counts, events, end, time.Second, p16Run)
	s.logf("%s", b.String())
	s.freshJudge(v, "a block", "minted it", p16P50Slack, p16P90Slack)
}

// freshJudge flags a new image later than the old one past the slacks, or
// reading the tag from its upstreams (polls and forwarded client reads)
// more than freshCostSlack times as often as the old one.
func (s *scenario) freshJudge(v map[string]freshVerdict, what, made string, p50Slack, p90Slack time.Duration) {
	o, n := v["old"], v["new"]
	if n.p50 > o.p50+p50Slack || n.p90 > o.p90+p90Slack {
		s.violate(s.name, "new image first answered %s p50 %s p90 %s after chainsim %s, old p50 %s p90 %s", what, n.p50, n.p90, made, o.p50, o.p90)
	}
	if n.reads > o.reads*freshCostSlack {
		s.violate(s.name, "new image read the tag from its upstreams %.2f times per unit, old %.2f", n.reads, o.reads)
	}
}

const (
	// p17Scale: 1.5 s blocks, reader 125 ms / 12.5 ms, vendors 1.25 s.
	p17Scale = 8
	// p17Depth, p17Step: each vendor's finalized block is p17Depth below
	// its head, rounded down to a multiple of p17Step: an Ethereum epoch's
	// step against its finality depth (32 of 64-96 blocks), so the
	// depth-widened finalized debounce stays the same share of a step.
	p17Depth = 32
	p17Step  = 16
	p17Run   = 5 * time.Minute
	// p17P50Slack, p17P90Slack: how much later the new image may first
	// answer a finality step (2 s and 4 s on Ethereum).
	p17P50Slack = 250 * time.Millisecond
	p17P90Slack = 500 * time.Millisecond
	// p17NumberedSlack: by-number block reads per finalized-tag client
	// read the new image may send above the old image's: with a cache,
	// one per finality step, not one per read.
	p17NumberedSlack = 0.05
)

// P17: P16's layout, time-scaled by p17Scale, with finality moving in
// steps and a cache; one client per image reads
// eth_getBlockByNumber("finalized") every reader poll interval. Per step,
// the time from chainsim's step to each pod first answering it: the new
// image is no later than the old, at no more finalized polls, and its
// reads of the finalized block by number hit the cache.
func p17() spec {
	sp := freshSpec("P17", p17Scale, chainsim.Config{FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, p17Body)
	sp.cache = true
	return sp
}

func p17Body(s *scenario) {
	s.blocks(10)
	start := time.Now()
	period := time.Second / p17Scale
	imgs, counts := s.freshRun(p17Run, period, "finalized", func(pod int) (uint64, error) {
		var raw map[string]interface{}
		if err := s.call(pod, &raw, "eth_getBlockByNumber", "finalized", false); err != nil {
			return 0, err
		}
		return hexutil.DecodeUint64(fmtString(raw["number"]))
	})
	end := time.Now()
	var events []freshFirst
	prev := uint64(0)
	for _, b := range s.chain.MintedBetween(start, end.Add(-3*s.chain.BlockTime())) {
		f := s.chain.Finalized(b.Number, p17Depth)
		if prev != 0 && f > prev {
			events = append(events, freshFirst{f, b.MintedAt})
		}
		prev = f
	}
	var b strings.Builder
	v := s.freshReport(&b, "steps", "finalized", imgs, counts, events, end, time.Minute, p17Run)
	s.logf("%s", b.String())
	if len(events) == 0 {
		s.violate(s.name, "chainsim made no finality step in %s", p17Run)
	}
	s.freshJudge(v, "a finality step", "made it", p17P50Slack, p17P90Slack)
	if o, n := v["old"], v["new"]; n.numberedPerRead > o.numberedPerRead+p17NumberedSlack {
		s.violate(s.name, "new image sent %.3f by-number block reads per finalized-tag read, old %.3f", n.numberedPerRead, o.numberedPerRead)
	}
}
