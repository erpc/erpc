package realworld

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/erpc/erpc/test/chainsim"
)

// freshUp is one https upstream of a freshness layout, its times on its
// chain's clock, before the layout's scale: its poll interval and
// debounce (0 the default), and how long after a block is minted its node
// serves it.
type freshUp struct {
	key, vendor        string
	slow               time.Duration
	interval, debounce time.Duration
	delay              time.Duration
}

// freshUps: an own reader polled every 1 s with a 100 ms debounce, and
// vendors every 10 s.
var freshUps = []freshUp{
	{"http-r", "reader", 2 * time.Millisecond, time.Second, 100 * time.Millisecond, 0},
	{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-b", "vendor-b", 30 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-c", "vendor-c", 25 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-d", "vendor-d", 40 * time.Millisecond, 10 * time.Second, 0, 0},
}

// freshSpec is a freshness layout ups on Kubernetes' times divided by
// scale: sim's block time (Ethereum's 12 s when 0), no ws source, quorum
// 2, one old-image and one new-image pod with their own state.
func freshSpec(name string, scale int, ups []freshUp, sim chainsim.Config, faults chainsim.Faults, body func(*scenario)) spec {
	d := func(t time.Duration) string { return (t / time.Duration(scale)).String() }
	var vendors []vendorSpec
	fs := map[string]chainsim.Faults{}
	upEvm := map[string]map[string]interface{}{}
	for _, u := range ups {
		vendors = append(vendors, vendorSpec{u.key, u.vendor, false})
		f := faults
		f.Slow, f.HeadDelay = u.slow, u.delay/time.Duration(scale)
		fs[u.key] = f
		upEvm[u.key] = map[string]interface{}{"statePollerInterval": d(u.interval)}
		if u.debounce > 0 {
			upEvm[u.key]["statePollerDebounce"] = d(u.debounce)
		}
	}
	if sim.BlockTime == 0 {
		sim.BlockTime = 12 * time.Second
	}
	sim.BlockTime /= time.Duration(scale)
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
	return freshSpec("P16", p16Scale, freshUps, chainsim.Config{}, chainsim.Faults{}, p16Body)
}

// freshImage is one image's tally: every answer, when it arrived and how
// long its request took.
type freshImage struct {
	pod     int
	mu      sync.Mutex
	answers []freshFirst
	took    []time.Duration
	errs    int
}

// freshFirst is a value and a time: an answer, or a chain event.
type freshFirst struct {
	n  uint64
	at time.Time
}

func (im *freshImage) saw(n uint64, at time.Time, took time.Duration) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.answers = append(im.answers, freshFirst{n, at})
	im.took = append(im.took, took)
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

// freshClient calls read on im's pod about every period until ctx ends.
// Each gap is uniform in [period/2, 3*period/2]: a fixed period dividing
// the block time would pin the reads to one phase of the chain clock per
// run, and that phase, not the image, would decide the delay.
func (s *scenario) freshClient(ctx context.Context, im *freshImage, period time.Duration, read func(pod int) (uint64, error)) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wait := time.NewTimer(period/2 + rand.N(period))
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
		}
		wait.Reset(period/2 + rand.N(period))
		wg.Add(1)
		go func() {
			defer wg.Done()
			sent := time.Now()
			n, err := read(im.pod)
			at := time.Now()
			if err != nil {
				im.mu.Lock()
				im.errs++
				im.mu.Unlock()
				return
			}
			im.saw(n, at, at.Sub(sent))
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
	for _, u := range s.spec.vendors {
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

// freshLoad is one client per pod: read every period; tag names the
// polls and tag reads its traffic report counts.
type freshLoad struct {
	period time.Duration
	tag    string
	read   func(s *scenario, pod int) (uint64, error)
}

// freshHead reads pod's eth_blockNumber.
func freshHead(s *scenario, pod int) (uint64, error) {
	var bn hexutil.Uint64
	err := s.call(pod, &bn, "eth_blockNumber")
	return uint64(bn), err
}

// freshFinalized reads the number of pod's finalized block.
func freshFinalized(s *scenario, pod int) (uint64, error) {
	var raw map[string]interface{}
	if err := s.call(pod, &raw, "eth_getBlockByNumber", "finalized", false); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(fmtString(raw["number"]))
}

// freshRun runs loads for run, one client per pod each, and returns, per
// load, each image's tally and its upstream traffic over the run.
func (s *scenario) freshRun(run time.Duration, loads ...freshLoad) (imgs [][]*freshImage, counts [][]freshCount) {
	ctx, cancel := context.WithTimeout(s.ctx, run)
	defer cancel()
	before := make([][]freshCount, len(loads))
	imgs = make([][]*freshImage, len(loads))
	for i, l := range loads {
		for _, p := range s.h.pods {
			imgs[i] = append(imgs[i], &freshImage{pod: p.idx})
			before[i] = append(before[i], s.freshCounts(p.idx, l.tag))
		}
	}
	var wg sync.WaitGroup
	for i, l := range loads {
		for _, im := range imgs[i] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.freshClient(ctx, im, l.period, func(pod int) (uint64, error) { return l.read(s, pod) })
			}()
		}
	}
	wg.Wait()
	counts = make([][]freshCount, len(loads))
	for i, l := range loads {
		for j, p := range s.h.pods {
			counts[i] = append(counts[i], s.freshCounts(p.idx, l.tag).since(before[i][j]))
		}
	}
	return imgs, counts
}

// freshBlocks are the blocks minted in [from, to], as events at their mint.
func (s *scenario) freshBlocks(from, to time.Time) (events []freshFirst) {
	for _, b := range s.chain.MintedBetween(from, to) {
		events = append(events, freshFirst{b.Number, b.MintedAt})
	}
	return events
}

// freshSteps are the finality steps of vendors at finality depth depth
// in [from, to]: each new finalized number, at the mint of the block that
// moved it.
func (s *scenario) freshSteps(from, to time.Time, depth uint64) (events []freshFirst) {
	prev := uint64(0)
	for _, b := range s.chain.MintedBetween(from, to) {
		f := s.chain.Finalized(b.Number, depth)
		if prev != 0 && f > prev {
			events = append(events, freshFirst{f, b.MintedAt})
		}
		prev = f
	}
	return events
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
		for _, u := range s.spec.vendors {
			p, t, r := counts[i].polls[u.key]/per, counts[i].reads[u.key]/per, counts[i].requests[u.key]/per
			v.polls += p
			v.reads += t
			v.requests += r
			numbered += counts[i].numbered[u.key]
			fmt.Fprintf(b, " %s %.2f/%.2f/%.2f", u.name, p, t, r)
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
	imgs, counts := s.freshRun(p16Run, freshLoad{time.Second / p16Rate, "latest", freshHead})
	end := time.Now()
	events := s.freshBlocks(start.Add(s.chain.BlockTime()), end.Add(-s.chain.BlockTime()))
	var b strings.Builder
	v := s.freshReport(&b, "blocks", "latest", imgs[0], counts[0], events, end, time.Second, p16Run)
	s.logf("%s", b.String())
	s.freshJudge(v, "a block", "minted it", p16P50Slack, p16P90Slack)
}

const (
	// p16SpreadDelay: each P16-spread vendor serves each block this long
	// after its mint, on Ethereum's clock, plus a share of
	// p16SpreadJitter, already scaled, fixed per vendor and block: 0.8 s
	// plus up to 2.0 s, so first arrivals spread as mainnet's do (p50
	// about 1.8 s).
	p16SpreadDelay  = 800 * time.Millisecond
	p16SpreadJitter = 250 * time.Millisecond
	// p16Blip, p16BlipEvery: vendor-a is black-holed 2 s every 20 s on
	// Ethereum's clock.
	p16Blip      = 250 * time.Millisecond
	p16BlipEvery = 2500 * time.Millisecond
	// p16BlipSlack: how much older the new image's served latest p90 may
	// be inside the blips than outside them: one rung spacing of the
	// block clock's ladder (about 0.33 s on mainnet), scaled.
	p16BlipSlack = 40 * time.Millisecond
	// p16IrregularBlockTime, p16IrregularJitter: Linea's blocks, 2-12 s
	// apart, on Ethereum's clock.
	p16IrregularBlockTime = 7 * time.Second
	p16IrregularJitter    = 5 * time.Second
)

// p16SpreadUps are freshUps' four vendors, without the reader: mainnet
// has no sub-block reader. Each serves each block p16SpreadDelay after
// its mint, plus its jitter.
func p16SpreadUps() []freshUp {
	var ups []freshUp
	for _, u := range freshUps {
		if u.key != "http-r" {
			u.delay = p16SpreadDelay
			ups = append(ups, u)
		}
	}
	return ups
}

// P16-spread: P16's chain and vendors as mainnet's (p16SpreadUps), per
// pod p16Rate head reads a second, half eth_blockNumber and half
// eth_getBlockByNumber(latest), for p16Run. The new image's served
// latest age and latency within P16's slacks of the old image's (main
// reads latest live from an upstream), at most turnBound head fetches per
// new block, no more upstream requests. P16-spread-blip: vendor-a
// black-holed p16Blip every p16BlipEvery; inside the blips the new
// image's served age p90 at most its p90 outside + p16BlipSlack.
func p16Spread(blip bool) spec {
	name := "P16-spread"
	if blip {
		name = "P16-spread-blip"
	}
	ups := p16SpreadUps()
	return freshSpec(name, p16Scale, ups, chainsim.Config{}, chainsim.Faults{HeadJitter: p16SpreadJitter}, func(s *scenario) { p16SpreadBody(s, ups, blip) })
}

// P16-irregular: P16-spread on Linea's irregular blocks,
// p16IrregularBlockTime ± p16IrregularJitter apart.
func p16Irregular() spec {
	ups := p16SpreadUps()
	sim := chainsim.Config{BlockTime: p16IrregularBlockTime, MintJitter: p16IrregularJitter / p16Scale}
	return freshSpec("P16-irregular", p16Scale, ups, sim, chainsim.Faults{HeadJitter: p16SpreadJitter}, func(s *scenario) { p16SpreadBody(s, ups, false) })
}

func p16SpreadBody(s *scenario, ups []freshUp, blip bool) {
	s.blocks(10)
	vl := &viewLog{}
	reads := liveReads(p16Rate, s.chain.BlockTime(), vl)
	for i := range reads {
		reads[i].slack = [2]time.Duration{p16P50Slack, p16P90Slack}
	}
	var fault func(context.Context, *faultWindows)
	if blip {
		hole := func(on bool) {
			for _, p := range s.h.pods {
				s.proxies["http-a"].set(p, linkFaults{BlackHole: on})
			}
		}
		fault = func(ctx context.Context, w *faultWindows) {
			for {
				sleepCtx(ctx, p16BlipEvery-p16Blip)
				if ctx.Err() != nil {
					return
				}
				w.fault(ctx, p16Blip, func() { hole(true) }, func() { hole(false) })
			}
		}
	}
	before := s.clockMix()
	imgs, all, w, from, to := s.liveRun(p16Run, reads, fault)
	var b strings.Builder
	s.judgeAgeLatency(&b, reads, imgs, from, to)
	s.judgeFetches(&b, "over the run", all, turnBound(ups, p16Scale, s.chain.BlockTime()))
	s.judgeRequests(&b, all)
	reportClockMix(&b, before, s.clockMix(), all.blocks)
	if blip {
		for i, rd := range reads {
			var ims []*freshImage
			for _, im := range imgs[i] {
				if !s.h.pods[im.pod].old {
					ims = append(ims, im)
				}
			}
			in := s.servedAges(ims, w.inside)
			out := s.servedAges(ims, func(t time.Time) bool { return !w.inside(t) })
			fmt.Fprintf(&b, "new image %s served age inside the blips p50 %s p90 %s (%d), outside p50 %s p90 %s (%d)\n",
				rd.name, pctl(in, 50), pctl(in, 90), len(in), pctl(out, 50), pctl(out, 90), len(out))
			if pctl(in, 90) > pctl(out, 90)+p16BlipSlack {
				s.violate(s.name, "new image %s served age p90 %s inside the blips, %s outside: more than %s older", rd.name, pctl(in, 90), pctl(out, 90), p16BlipSlack)
			}
		}
	}
	s.logf("%s", b.String())
}

// turnBound is spec §6.8's head fetch budget per new block of a lane
// whose voters ups are polled every interval, divided by scale, on a
// chain of block time bt: each voter's turn at most once a block and
// once an interval, at least one fetch a block, plus 5% for tail rungs
// and hedges.
func turnBound(ups []freshUp, scale int, bt time.Duration) float64 {
	var turns float64
	for _, u := range ups {
		if iv := u.interval / time.Duration(scale); iv > 0 {
			turns += min(1, bt.Seconds()/iv.Seconds())
		}
	}
	return max(1, turns) + 0.05
}

// clockMix is, per new-image pod, where its head fetches went: its block
// clock's Asks by kind and outcome (erpc_head_clock_total) and its head
// Asks that fetched, by reason (erpc_head_ask_total).
func (s *scenario) clockMix() map[int]map[string]float64 {
	out := map[int]map[string]float64{}
	for _, p := range s.h.pods {
		if p.old {
			continue
		}
		ss, err := p.scrape()
		if err != nil {
			continue
		}
		m := map[string]float64{}
		for _, x := range ss {
			if x.labels["project"] != s.project || x.labels["network"] != s.network() {
				continue
			}
			switch {
			case x.name == "erpc_head_clock_total":
				m["clock "+x.labels["kind"]+" "+x.labels["outcome"]] += x.value
			case x.name == "erpc_head_ask_total" && x.labels["outcome"] == "fetched":
				m["ask "+x.labels["reason"]+" fetched"] += x.value
			}
		}
		out[p.idx] = m
	}
	return out
}

// reportClockMix writes each pod's clockMix between before and after per
// new block, over blocks.
func reportClockMix(b *strings.Builder, before, after map[int]map[string]float64, blocks uint64) {
	if blocks == 0 {
		return
	}
	for pod, m := range after {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Fprintf(b, "pod-%d head fetches per new block by cause:", pod+1)
		for _, k := range keys {
			if d := m[k] - before[pod][k]; d > 0 {
				fmt.Fprintf(b, " %s %.3f;", k, d/float64(blocks))
			}
		}
		b.WriteString("\n")
	}
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
	// p17SlowRead: a finalized-tag read slower than this waited or went
	// past the cache and the nearest upstream; p17SlowSlack: the share of
	// such reads the new image may take above the old image's.
	p17SlowRead  = 50 * time.Millisecond
	p17SlowSlack = 0.01
	// p17AvaxBlockTime: Avalanche's C-chain blocks, real time;
	// p17AvaxRate: each image's finalized-tag reads a second;
	// p17AvaxHeadEvery: the period of each of its two head-read clients,
	// 7.5 reads a second in all: a measured avalanche head-read mix.
	p17AvaxBlockTime = 1200 * time.Millisecond
	p17AvaxRate      = 3
	p17AvaxHeadEvery = 2 * time.Second * 2 / 15
)

// p17AvaxUps are Avalanche's voters on Kubernetes: a near holder 5 ms
// away whose headers no format encodes (Prism's, BareHeaders), and seven
// far voters 75-250 ms away, every one polled every 5 s.
var p17AvaxUps = []freshUp{
	{"http-r", "reader", 5 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-a", "vendor-a", 75 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-b", "vendor-b", 100 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-c", "vendor-c", 125 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-d", "vendor-d", 150 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-e", "vendor-e", 175 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-f", "vendor-f", 200 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-g", "vendor-g", 250 * time.Millisecond, 5 * time.Second, 0, 0},
}

// P17: P16's layout, time-scaled by p17Scale, with finality moving in
// steps and a cache; one client per image reads
// eth_getBlockByNumber("finalized") every reader poll interval. Per step,
// the time from chainsim's step to each pod first answering it: the new
// image is no later than the old, at no more finalized polls, its reads
// of the finalized block by number hit the cache, and no larger share of
// its finalized-tag reads is slow (p17SlowRead).
func p17() spec {
	sp := freshSpec("P17", p17Scale, freshUps, chainsim.Config{FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth},
		func(s *scenario) { p17Judge(s, p17Body(s, time.Second/p17Scale, p17Depth)) })
	sp.cache = true
	return sp
}

// P17-avax: P17's verdicts on Avalanche: 1.2 s blocks in real time,
// finality one block below the head, so it moves every block;
// p17AvaxUps; per image p17AvaxRate finalized-tag reads a second beside
// eth_blockNumber and eth_getBlockByNumber(latest) every
// p17AvaxHeadEvery each. The new image holds the near holder's headers
// unverified.
func p17Avax() spec {
	sp := freshSpec("P17-avax", 1, p17AvaxUps, chainsim.Config{BlockTime: p17AvaxBlockTime}, chainsim.Faults{FinalityDepth: 1}, func(s *scenario) {
		p17Judge(s, p17Body(s, time.Second/p17AvaxRate, 1, freshLoad{p17AvaxHeadEvery, "latest", freshHead}, freshLoad{p17AvaxHeadEvery, "latest", freshLatestBlock}))
		for pod := range s.h.pods {
			if s.image(pod) != "new" {
				continue
			}
			if v, _ := s.metric(pod, "erpc_chain_tracker_header_proof_total", map[string]string{"proof": "unverified", "reason": "unencodable"}); v == 0 {
				s.violate(s.name, "pod-%d held no unverified header: the near holder's bare headers never reached its tracker", pod+1)
			}
		}
	})
	sp.cache = true
	f := sp.faults["http-r"]
	f.BareHeaders = true
	sp.faults["http-r"] = f
	return sp
}

// p17CeloUps are celo's measured live voters: a near reader whose
// finalized tag answers unsupported (Prism's), polled every 5 s, and one
// far vendor polled every 10 s.
var p17CeloUps = []freshUp{
	{"http-r", "reader", 5 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-a", "vendor-a", 100 * time.Millisecond, 10 * time.Second, 0, 0},
}

const (
	// p17CeloDepth, p17CeloStep: OP-stack finality on celo's 1 s blocks,
	// minutes behind the head, moving an L1 block (12 s) at a time;
	// p17CeloHistory keeps the blocks a voter without the tag derives (30
	// min below its head).
	p17CeloDepth   = 600
	p17CeloStep    = 12
	p17CeloHistory = 2000
	// p17CeloEvery: the period of each image's finalized-tag and
	// eth_blockNumber clients, the measured celo reads (another eRPC's state
	// polls of it, about every 3 s): about W apart.
	p17CeloEvery = 3 * time.Second
)

// P17-celo: P17's step delays on celo: 1 s blocks in real time, finality
// p17CeloDepth deep in p17CeloStep steps, p17CeloUps (the reader's
// finalized tag unsupported, so given up), quorum 2; per image one
// finalized-tag and one eth_blockNumber client every p17CeloEvery. The
// old image forwards each tag read upstream, so its share of slow reads
// is no reference: the new image serves the tag from its Head and cache,
// so at most p17SlowSlack of its reads are slow (a read that waits for a
// finalized poll is). With one read per W, a step every 4 reads may cost
// a by-number read of the new finalized block, so the cost bound is the
// tag's whole upstream traffic: the new image's finalized polls and
// by-number block reads at most freshCostSlack times the old image's
// finalized-tag requests.
func p17Celo() spec {
	sim := chainsim.Config{BlockTime: time.Second, FinalityStep: p17CeloStep, History: p17CeloHistory}
	sp := freshSpec("P17-celo", 1, p17CeloUps, sim, chainsim.Faults{FinalityDepth: p17CeloDepth}, func(s *scenario) {
		r := p17Body(s, p17CeloEvery, p17CeloDepth, freshLoad{p17CeloEvery, "latest", freshHead})
		perMin := float64(time.Minute) / float64(p17Run)
		cost := func(img string) float64 { return r.v[img].reads + r.numbered[img]*perMin }
		if n, o := cost("new"), cost("old"); n > o*freshCostSlack {
			s.violate(s.name, "new image sent %.2f finalized-tag and by-number block reads a minute, old %.2f", n, o)
		}
		if r.slow["new"] > p17SlowSlack {
			s.violate(s.name, "%.1f%% of the new image's finalized-tag reads took over %s", 100*r.slow["new"], p17SlowRead)
		}
	})
	sp.cache = true
	f := sp.faults["http-r"]
	f.RefuseFinalized = true
	sp.faults["http-r"] = f
	return sp
}

// p17Result is p17Body's tally per image: its step verdict, its by-number
// block reads, its share of finalized-tag reads over p17SlowRead; and the
// run's finality steps.
type p17Result struct {
	v              map[string]freshVerdict
	numbered, slow map[string]float64
	steps          int
}

// p17Body reads the finalized tag every period beside extra loads and
// judges the step delays and the tag's upstream reads against the old
// image's.
func p17Body(s *scenario, period time.Duration, depth uint64, extra ...freshLoad) p17Result {
	s.blocks(10)
	start := time.Now()
	imgs, counts := s.freshRun(p17Run, append([]freshLoad{{period, "finalized", freshFinalized}}, extra...)...)
	end := time.Now()
	events := s.freshSteps(start, end.Add(-3*s.chain.BlockTime()), depth)
	var b strings.Builder
	r := p17Result{v: s.freshReport(&b, "steps", "finalized", imgs[0], counts[0], events, end, time.Minute, p17Run),
		numbered: map[string]float64{}, slow: map[string]float64{}, steps: len(events)}
	for i, im := range imgs[0] {
		for _, u := range s.spec.vendors {
			r.numbered[s.image(im.pod)] += counts[0][i].numbered[u.key]
		}
		im.mu.Lock()
		n := 0
		for _, d := range im.took {
			if d > p17SlowRead {
				n++
			}
		}
		if len(im.took) > 0 {
			r.slow[s.image(im.pod)] = float64(n) / float64(len(im.took))
		}
		fmt.Fprintf(&b, "pod-%d (%s image): %d of %d finalized-tag reads over %s\n", im.pod+1, s.image(im.pod), n, len(im.took), p17SlowRead)
		im.mu.Unlock()
	}
	s.logf("%s", b.String())
	if len(events) == 0 {
		s.violate(s.name, "chainsim made no finality step in %s", p17Run)
	}
	s.freshJudge(r.v, "a finality step", "made it", p17P50Slack, p17P90Slack)
	return r
}

// p17Judge flags the new image reading the finalized block by number more
// than p17NumberedSlack per finalized-tag read above the old image, or
// taking a larger share of slow finalized-tag reads past p17SlowSlack.
func p17Judge(s *scenario, r p17Result) {
	if o, n := r.v["old"], r.v["new"]; n.numberedPerRead > o.numberedPerRead+p17NumberedSlack {
		s.violate(s.name, "new image sent %.3f by-number block reads per finalized-tag read, old %.3f", n.numberedPerRead, o.numberedPerRead)
	}
	if r.slow["new"] > r.slow["old"]+p17SlowSlack {
		s.violate(s.name, "%.1f%% of the new image's finalized-tag reads took over %s, old %.1f%%", 100*r.slow["new"], p17SlowRead, 100*r.slow["old"])
	}
}

const (
	// p18Scale: 1.5 s blocks, as P17.
	p18Scale = 8
	// p18HeadRate, p18LatestRate, p18FinalizedRate: each pod's
	// eth_blockNumber, eth_getBlockByNumber("latest") and
	// eth_getBlockByNumber("finalized") reads per second of Ethereum
	// time (7.5, 2 and 1), scaled.
	p18HeadRate      = 60
	p18LatestRate    = 16
	p18FinalizedRate = 8
	p18Run           = 8 * time.Minute
	// p18CostSlack: the new image may send its upstreams at most this
	// many times the old image's requests.
	p18CostSlack = 1.10
)

// p18Ups are eight voters: a reader that serves each block delay after
// the vendors, six vendors polled every 10 s and one every 5 s.
func p18Ups(delay time.Duration) []freshUp {
	return []freshUp{
		{"http-r", "reader", 2 * time.Millisecond, time.Second, 100 * time.Millisecond, delay},
		{"http-a", "vendor-a", 20 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-b", "vendor-b", 30 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-c", "vendor-c", 25 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-d", "vendor-d", 40 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-e", "vendor-e", 35 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-f", "vendor-f", 45 * time.Millisecond, 10 * time.Second, 0, 0},
		{"http-g", "vendor-g", 10 * time.Millisecond, 5 * time.Second, 0, 0},
	}
}

// P18: Ethereum on Kubernetes at 1/8 time: a reader that
// serves each block delay (on Ethereum's clock) after the vendors and
// answers only state methods, so block reads go to vendors; seven
// vendors, P17's stepping finality, a cache, quorum 2; per pod
// eth_blockNumber, eth_getBlockByNumber("latest") and
// eth_getBlockByNumber("finalized") clients at busy rates. The new
// image answers each block and each finality step no later than the old
// one, at no more upstream requests.
func p18(delay time.Duration) spec {
	sp := freshSpec(fmt.Sprintf("P18-d%s", delay), p18Scale, p18Ups(delay), chainsim.Config{FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, p18Body)
	sp.cache = true
	sp.up = map[string]map[string]interface{}{"http-r": {
		"allowMethods":  []string{"eth_call", "eth_getBalance", "eth_getCode", "eth_getStorageAt", "eth_getTransactionCount", "eth_chainId"},
		"ignoreMethods": []string{"*"},
		// Preferred over the vendors.
		"routing": map[string]interface{}{"scoreMultipliers": []map[string]interface{}{{"network": "", "method": "", "overall": 2}}},
	}}
	return sp
}

// freshLatestBlock reads the number of pod's latest block.
func freshLatestBlock(s *scenario, pod int) (uint64, error) {
	var raw map[string]interface{}
	if err := s.call(pod, &raw, "eth_getBlockByNumber", "latest", false); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(fmtString(raw["number"]))
}

func p18Body(s *scenario) {
	s.blocks(10)
	start := time.Now()
	imgs, counts := s.freshRun(p18Run,
		freshLoad{time.Second / p18HeadRate, "latest", freshHead},
		freshLoad{time.Second / p18FinalizedRate, "finalized", freshFinalized},
		freshLoad{time.Second / p18LatestRate, "latest", freshLatestBlock})
	end := time.Now()
	var b strings.Builder
	head := s.freshReport(&b, "blocks", "latest", imgs[0], counts[0], s.freshBlocks(start.Add(s.chain.BlockTime()), end.Add(-s.chain.BlockTime())), end, time.Second, p18Run)
	steps := s.freshSteps(start, end.Add(-3*s.chain.BlockTime()), p17Depth)
	fin := s.freshReport(&b, "steps", "finalized", imgs[1], counts[1], steps, end, time.Second, p18Run)
	s.logf("%s", b.String())
	if len(steps) == 0 {
		s.violate(s.name, "chainsim made no finality step in %s", p18Run)
	}
	for _, c := range []struct {
		v          map[string]freshVerdict
		what, made string
		p50, p90   time.Duration
	}{
		{head, "a block", "minted it", p16P50Slack, p16P90Slack},
		{fin, "a finality step", "made it", p17P50Slack, p17P90Slack},
	} {
		if o, n := c.v["old"], c.v["new"]; n.p50 > o.p50+c.p50 || n.p90 > o.p90+c.p90 {
			s.violate(s.name, "new image first answered %s p50 %s p90 %s after chainsim %s, old p50 %s p90 %s", c.what, n.p50, n.p90, c.made, o.p50, o.p90)
		}
	}
	if o, n := head["old"], head["new"]; n.requests > o.requests*p18CostSlack {
		s.violate(s.name, "new image sent its upstreams %.2f requests/s, old %.2f", n.requests, o.requests)
	}
}

const (
	// p19BlockTime: Arbitrum's blocks, in real time.
	p19BlockTime = 250 * time.Millisecond
	p19Run       = 8 * time.Minute
	// p19P50Slack, p19P90Slack: how much older the new image's served
	// latest block, and how much slower its answer, may be than the old
	// image's: a fifth and two fifths of a block.
	p19P50Slack = 50 * time.Millisecond
	p19P90Slack = 100 * time.Millisecond
)

// p19Ups are seven voters, every one polled over https:
// four vendors at 4-6 s, three at 10 s.
var p19Ups = []freshUp{
	{"http-a", "vendor-a", 20 * time.Millisecond, 4 * time.Second, 0, 0},
	{"http-b", "vendor-b", 30 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-c", "vendor-c", 25 * time.Millisecond, 5 * time.Second, 0, 0},
	{"http-d", "vendor-d", 40 * time.Millisecond, 6 * time.Second, 0, 0},
	{"http-e", "vendor-e", 35 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-f", "vendor-f", 45 * time.Millisecond, 10 * time.Second, 0, 0},
	{"http-g", "vendor-g", 10 * time.Millisecond, 10 * time.Second, 0, 0},
}

// P19: Arbitrum's 0.25 s blocks, in real time: seven vendors
// polled every 4-10 s, P17's stepping finality, a cache, quorum 2; per
// pod, rate head reads a second, half eth_blockNumber and half
// eth_getBlockByNumber("latest"), and rate/4 eth_getBlockByNumber
// ("finalized"). Per method, each answer's block age (from chainsim
// minting it to the answer) and request latency: the new image's are
// within the slacks of the old one's, and at one read per W or more its
// head-read latency p50 and p90 at most the old one's (liveReads). The
// new image sends at most 1.05 head fetches per
// new block, and at one read per block or more no more upstream requests
// than the old one. No source pushes, so no pod takes a newHeads
// subscription: X10 holds HTTP against the stream.
func p19(rate float64) spec {
	sp := freshSpec(fmt.Sprintf("P19-r%g", rate), 1, p19Ups, chainsim.Config{BlockTime: p19BlockTime, FinalityStep: p17Step}, chainsim.Faults{FinalityDepth: p17Depth}, func(s *scenario) { p19Body(s, rate) })
	sp.cache = true
	return sp
}

func p19Body(s *scenario, rate float64) {
	s.blocks(10)
	vl := &viewLog{}
	reads := append(liveReads(rate, s.chain.BlockTime(), vl), liveRead{"eth_getBlockByNumber(finalized)",
		freshLoad{time.Duration(float64(4*time.Second) / rate), "finalized", freshFinalized}, [2]time.Duration{p17P50Slack, p17P90Slack}, false})
	imgs, all, _, from, to := s.liveRun(p19Run, reads, nil)
	var b strings.Builder
	s.judgeAgeLatency(&b, reads, imgs, from, to)
	s.judgeFetches(&b, "over the run", all, liveFetchBound)
	if rate*s.chain.BlockTime().Seconds() >= 1 {
		s.judgeRequests(&b, all)
	}
	s.logf("%s", b.String())
}
