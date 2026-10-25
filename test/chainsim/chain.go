package chainsim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// HistoryBlocks are minted at start, one block time apart, ending now,
	// unless Config.History says otherwise.
	HistoryBlocks = 64
	firstNumber   = 0x1000
	// FinalizedLag is how far below its head a vendor's finalized block is,
	// unless Faults.FinalityDepth says otherwise.
	FinalizedLag = 64
)

// Config is one simulated chain.
type Config struct {
	ChainID   int64
	BlockTime time.Duration
	Seed      uint64
	// HeaderPad pads every header's extraData by this many bytes, so few
	// headers fill a client's socket and flow-control windows.
	HeaderPad int
	// History is how many blocks are minted at start; 0 is HistoryBlocks.
	History int
	// NoFinalized: the chain has no finalized tag; every vendor refuses it.
	NoFinalized bool
	// BadBaseFee: every block's base fee is 1000 + its number, which no
	// parent derives (baseFeeDerivation).
	BadBaseFee bool
	// PendingNoise drops a quarter of the pending-transaction pushes and
	// sends a quarter twice.
	PendingNoise bool
	// MatchLast: every matching block ends with a log both filters match,
	// in a transaction with another log, so Faults.CapLogs can cut it.
	MatchLast bool
}

// Chain is one simulated chain: its canonical branch, every block ever
// minted, and the vendors serving it. One lock guards the chain and every
// vendor's view and faults.
type Chain struct {
	cfg Config

	mu      sync.Mutex
	rng     *rand.Rand
	noise   *rand.Rand
	minted  uint64
	canon   []*Block
	byHash  map[Hash]*Block
	halted  bool
	vendors []*Vendor
	forks   map[*Fork]bool
	// plan, while not empty, is the kind of the next blocks minted.
	plan []Kind
	// pushed are the transactions a vendor pushed as pending.
	pushed map[Hash]bool
	// trapHold, while set, holds each block's first receipts call; traps
	// are the calls held since it was set.
	trapHold time.Duration
	traps    map[Hash]*Trap
	// failHold, while set, makes the next canonical matching block failing,
	// whose receipts and logs calls fail until failUntil.
	failHold  time.Duration
	failing   *Block
	failUntil time.Time
	// headerBloomOff, while set, mints headers whose logsBloom is a
	// superset of the OR of their receipts' blooms (OffHeaderBloom).
	headerBloomOff bool
	// bigTxs, while set, is how many transactions each non-empty block
	// minted holds (BigBlocks).
	bigTxs int

	stop     chan struct{}
	stopOnce sync.Once
	started  atomic.Bool
	done     chan struct{}
	// ticks counts block times passed, halted ones included; tickCh is
	// closed and replaced at each.
	ticks  atomic.Uint64
	tickMu sync.Mutex
	tickCh chan struct{}
}

// Fork is a branch some vendors serve instead of the canonical chain; it
// gains a block every tick.
type Fork struct{ tip *Block }

// Trap is one block's held first receipts call.
type Trap struct {
	AnsweredAt time.Time // zero while held
	During     int       // other receipts calls for the block while it was held
}

// NewChain mints the history; Start makes blocks.
func NewChain(cfg Config) *Chain {
	c := &Chain{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, uint64(cfg.ChainID))), noise: rand.New(rand.NewPCG(cfg.Seed, uint64(cfg.ChainID)+1)),
		byHash: make(map[Hash]*Block), forks: make(map[*Fork]bool), pushed: make(map[Hash]bool), traps: make(map[Hash]*Trap),
		stop: make(chan struct{}), done: make(chan struct{}), tickCh: make(chan struct{})}
	history := cfg.History
	if history == 0 {
		history = HistoryBlocks
	}
	start := time.Now().Add(-time.Duration(history) * cfg.BlockTime)
	var parent *Block
	for i := range history {
		b := c.mintLocked(parent, firstNumber+uint64(i), start.Add(time.Duration(i)*cfg.BlockTime), false)
		c.canon = append(c.canon, b)
		parent = b
	}
	return c
}

// ID is the chain id.
func (c *Chain) ID() int64 { return c.cfg.ChainID }

// BlockTime is the chain's block time.
func (c *Chain) BlockTime() time.Duration { return c.cfg.BlockTime }

// Start makes a block every block time until Stop or Close.
func (c *Chain) Start() {
	c.started.Store(true)
	go func() {
		defer close(c.done)
		t := time.NewTicker(c.cfg.BlockTime)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.tick()
			}
		}
	}()
}

// Ticks is how many block times passed since Start, halted ones included:
// a block time the host was too slow to tick is not one.
func (c *Chain) Ticks() uint64 { return c.ticks.Load() }

// WaitTicks waits until n more block times passed, or the chain stopped.
func (c *Chain) WaitTicks(n uint64) {
	for to := c.Ticks() + n; c.Ticks() < to; {
		c.tickMu.Lock()
		ch := c.tickCh
		c.tickMu.Unlock()
		if c.Ticks() >= to {
			return
		}
		select {
		case <-ch:
		case <-c.stop:
			return
		}
	}
}

// Stop ends block production and waits for the tick in progress; vendors
// keep serving.
func (c *Chain) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
	if c.started.Load() {
		<-c.done
	}
}

// Close stops block production and every vendor.
func (c *Chain) Close() {
	c.Stop()
	c.mu.Lock()
	vs := slices.Clone(c.vendors)
	c.mu.Unlock()
	for _, v := range vs {
		v.close()
	}
}

func (c *Chain) mintLocked(parent *Block, number uint64, at time.Time, minority bool) *Block {
	c.minted++
	ts := uint64(at.Unix())
	if parent != nil && ts < parent.Time {
		ts = parent.Time
	}
	var k Kind
	if len(c.plan) > 0 {
		k, c.plan = c.plan[0], c.plan[1:]
	} else {
		switch r := c.rng.IntN(8); {
		case r == 0:
			k = KindEmpty
		case r <= 2:
			k = KindQuiet
		default:
			k = KindMatching
		}
	}
	txs := content(c.rng, k, c.bigTxs)
	if c.cfg.MatchLast && k == KindMatching {
		last := len(txs) - 1
		txs[last] = append(txs[last][:1], logSpec{addr: 0, topic: 1})
	}
	tag := fmt.Sprintf("%d/%d", c.cfg.Seed, c.minted)
	baseFee := int64(7)
	if c.cfg.BadBaseFee {
		baseFee = 1000 + int64(number)
	}
	hash, raw := header(c.cfg.ChainID, parent, number, ts, tag, txs, c.cfg.HeaderPad, baseFee, c.headerBloomOff)
	b := &Block{Number: number, Hash: hash, Parent: parent, Time: ts, MintedAt: time.Now(), minority: minority, kind: k, tag: tag, txs: txs, raw: raw}
	c.byHash[hash] = b
	if c.failHold > 0 && k == KindMatching && !minority {
		c.failing, c.failUntil, c.failHold = b, b.MintedAt.Add(c.failHold), 0
	}
	return b
}

func (c *Chain) tipLocked() *Block { return c.canon[len(c.canon)-1] }

func (c *Chain) canonAtLocked(n uint64) *Block {
	base := c.canon[0].Number
	if n < base || n-base >= uint64(len(c.canon)) {
		return nil
	}
	return c.canon[n-base]
}

// timeAtLocked is the time of a block at height n off the canonical chain:
// the canonical block's at n, or, above the head, when the chain reaches n.
func (c *Chain) timeAtLocked(n uint64) time.Time {
	if b := c.canonAtLocked(n); b != nil {
		return time.Unix(int64(b.Time), 0)
	}
	return time.Now().Add(time.Duration(int64(n)-int64(c.tipLocked().Number)) * c.cfg.BlockTime)
}

// Tip is the canonical head.
func (c *Chain) Tip() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tipLocked()
}

// CanonAt is the canonical block at n, nil outside the chain.
func (c *Chain) CanonAt(n uint64) *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canonAtLocked(n)
}

// MintedBetween are the canonical blocks minted in [from, to], oldest
// first.
func (c *Chain) MintedBetween(from, to time.Time) []*Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Block
	for _, b := range c.canon {
		if !b.MintedAt.Before(from) && !b.MintedAt.After(to) {
			out = append(out, b)
		}
	}
	return out
}

// Block is the minted block with hash h, canonical or not; nil if none.
func (c *Chain) Block(h Hash) *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byHash[h]
}

// CanonicalLogs are the logs f matches in canonical blocks from..to.
func (c *Chain) CanonicalLogs(f Filter, from, to uint64) map[LogKey]Log {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[LogKey]Log)
	for n := from; n <= to; n++ {
		b := c.canonAtLocked(n)
		if b == nil {
			continue
		}
		for _, l := range b.Logs() {
			if f.Matches(l) {
				out[l.Key()] = l
			}
		}
	}
	return out
}

// Pushed reports whether a vendor pushed tx as a pending transaction.
func (c *Chain) Pushed(tx Hash) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pushed[tx]
}

// Halt stops or resumes canonical block production.
func (c *Chain) Halt(halted bool) {
	c.mu.Lock()
	c.halted = halted
	c.mu.Unlock()
}

// Plan makes the next blocks minted, on any branch, of these kinds, ahead
// of the kinds planned before. It returns the canonical head as planned:
// while only ticks mint, the planned blocks follow it.
func (c *Chain) Plan(kinds ...Kind) (head *Block) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.plan = append(slices.Clone(kinds), c.plan...)
	return c.tipLocked()
}

// Append mints the next canonical block at once, of kind k, with its header
// JSON edited by edit; its hash stays the unedited header's.
func (c *Chain) Append(k Kind, edit func(fields map[string]json.RawMessage)) *Block {
	c.mu.Lock()
	c.plan = append([]Kind{k}, c.plan...)
	tip := c.tipLocked()
	b := c.mintLocked(tip, tip.Number+1, time.Now(), false)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.raw, &fields); err != nil {
		panic(err)
	}
	edit(fields)
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	b.raw = raw
	c.canon = append(c.canon, b)
	c.mu.Unlock()
	c.refresh()
	return b
}

// Orphan mints a minority child of parent that no vendor serves.
func (c *Chain) Orphan(parent *Block) *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mintLocked(parent, parent.Number+1, time.Now(), true)
}

// tick adds a canonical block unless the chain halted, grows every fork,
// and lets each vendor follow; wss vendors first push the new block's
// transactions as pending ones.
func (c *Chain) tick() {
	c.mu.Lock()
	var pushes []push
	if !c.halted {
		pushes = c.mintCanonLocked()
	}
	for f := range c.forks {
		f.tip = c.mintLocked(f.tip, f.tip.Number+1, c.timeAtLocked(f.tip.Number+1), true)
	}
	c.mu.Unlock()
	writePushes(pushes)
	c.refresh()
	c.ticks.Add(1)
	c.tickMu.Lock()
	close(c.tickCh)
	c.tickCh = make(chan struct{})
	c.tickMu.Unlock()
}

// Burst mints n canonical blocks spread evenly over d, halted or not;
// vendors follow each at once.
func (c *Chain) Burst(n int, d time.Duration) {
	for i := range n {
		if i > 0 {
			time.Sleep(d / time.Duration(n))
		}
		c.mu.Lock()
		pushes := c.mintCanonLocked()
		c.mu.Unlock()
		writePushes(pushes)
		c.refresh()
	}
}

// mintCanonLocked appends the next canonical block and returns the pushes
// of its transactions as pending ones.
func (c *Chain) mintCanonLocked() []push {
	b := c.mintLocked(c.tipLocked(), c.tipLocked().Number+1, time.Now(), false)
	c.canon = append(c.canon, b)
	var pushes []push
	for _, v := range c.vendors {
		pushes = v.pendingPushesLocked(b, pushes)
	}
	return pushes
}

// Reorg replaces the top depth canonical blocks with a new branch one block
// longer; the old blocks become orphans that vendors still serve by hash.
// It returns the replaced blocks, oldest first, and the new head.
func (c *Chain) Reorg(depth int) (old []*Block, tip *Block) {
	c.mu.Lock()
	keep := len(c.canon) - depth
	old = slices.Clone(c.canon[keep:])
	c.canon = c.canon[:keep]
	p := c.tipLocked()
	for _, o := range old {
		p = c.mintLocked(p, p.Number+1, time.Unix(int64(o.Time), 0), false)
		c.canon = append(c.canon, p)
	}
	c.canon = append(c.canon, c.mintLocked(p, p.Number+1, time.Now(), false))
	tip = c.tipLocked()
	c.mu.Unlock()
	c.refresh()
	return old, tip
}

// Rewind drops the top depth canonical blocks, as a chain whose nodes roll
// their head back does; the next blocks mint on the shortened chain. The
// dropped blocks become orphans that vendors still serve by hash. It
// returns the dropped blocks, oldest first, and the new head.
func (c *Chain) Rewind(depth int) (old []*Block, tip *Block) {
	c.mu.Lock()
	keep := len(c.canon) - depth
	old = slices.Clone(c.canon[keep:])
	c.canon = c.canon[:keep]
	tip = c.tipLocked()
	c.mu.Unlock()
	c.refresh()
	return old, tip
}

// StartFork moves vs onto a minority branch that leaves the canonical chain
// depth blocks below its head and runs length blocks from there; it grows
// every tick until EndFork.
func (c *Chain) StartFork(depth, length int, vs ...*Vendor) *Fork {
	c.mu.Lock()
	f := c.forkLocked(c.canonAtLocked(c.tipLocked().Number-uint64(depth)), length, vs)
	c.mu.Unlock()
	c.refresh()
	return f
}

// StartSplit mints the next canonical block and, for vs, a minority sibling
// of it in one step, so vs never serve the canonical one; the sibling's
// branch grows every tick until EndFork.
func (c *Chain) StartSplit(vs ...*Vendor) (canon *Block, f *Fork) {
	c.mu.Lock()
	tip := c.tipLocked()
	canon = c.mintLocked(tip, tip.Number+1, time.Now(), false)
	c.canon = append(c.canon, canon)
	f = c.forkLocked(tip, 1, vs)
	c.mu.Unlock()
	c.refresh()
	return canon, f
}

func (c *Chain) forkLocked(base *Block, length int, vs []*Vendor) *Fork {
	b := base
	for range length {
		b = c.mintLocked(b, b.Number+1, c.timeAtLocked(b.Number+1), true)
	}
	f := &Fork{tip: b}
	c.forks[f] = true
	for _, v := range vs {
		v.fork = f
	}
	return f
}

// StartDetached moves vs onto a minority branch that links to no block of
// the chain: its root, one below the canonical tip, has a zero parentHash.
// The branch runs at the canonical heights and grows every tick until
// EndFork.
func (c *Chain) StartDetached(vs ...*Vendor) *Fork {
	c.mu.Lock()
	tip := c.tipLocked()
	root := c.mintLocked(nil, tip.Number-1, time.Unix(int64(tip.Time), 0), true)
	f := c.forkLocked(root, 1, vs)
	c.mu.Unlock()
	c.refresh()
	return f
}

// BigBlocks makes each non-empty block minted from now hold txs
// transactions, receipts and logs scaled with them; 0 ends it.
func (c *Chain) BigBlocks(txs int) {
	c.mu.Lock()
	c.bigTxs = txs
	c.mu.Unlock()
}

// EndFork returns the fork's vendors to the canonical chain.
func (c *Chain) EndFork(f *Fork) {
	c.mu.Lock()
	delete(c.forks, f)
	for _, v := range c.vendors {
		if v.fork == f {
			v.fork = nil
		}
	}
	c.mu.Unlock()
	c.refresh()
}

// SetTrap holds the first eth_getBlockReceipts call of each block with
// transactions, on any vendor, for hold, then answers it with zero
// logsBlooms; 0 ends it. Setting it forgets the calls held before.
func (c *Chain) SetTrap(hold time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if hold > 0 {
		c.traps = make(map[Hash]*Trap)
	}
	c.trapHold = hold
}

// Traps are the receipts calls SetTrap held, by block hash.
func (c *Chain) Traps() map[Hash]Trap {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[Hash]Trap, len(c.traps))
	for h, tr := range c.traps {
		out[h] = *tr
	}
	return out
}

// FailNextMatching makes the next canonical block with matching logs fail
// every receipts and logs call on every vendor for hold after it is minted.
func (c *Chain) FailNextMatching(hold time.Duration) {
	c.mu.Lock()
	c.failHold, c.failing = hold, nil
	c.mu.Unlock()
}

// OffHeaderBloom, while on, mints a chain whose header bloom is not its
// receipts' OR: every block with transactions carries a logsBloom with one
// more value added, on every vendor, with receipts and
// receiptsRoot consistent, as a chain whose bloom rule differs from
// Ethereum's would serve. bloomMatch fails on every answer from every
// vendor, so the network suspends it (spec §6.9).
func (c *Chain) OffHeaderBloom(on bool) {
	c.mu.Lock()
	c.headerBloomOff = on
	c.mu.Unlock()
}

// Failing is the block FailNextMatching chose, nil until it is minted.
func (c *Chain) Failing() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failing
}

func (c *Chain) failingLocked(h Hash) bool {
	return c.failing != nil && c.failing.Hash == h && time.Now().Before(c.failUntil)
}

// refresh moves each vendor's tip to what its view serves and pushes each
// new tip on its newHeads subscriptions.
func (c *Chain) refresh() {
	c.mu.Lock()
	var pushes []push
	for _, v := range c.vendors {
		pushes = v.followLocked(pushes)
	}
	c.mu.Unlock()
	writePushes(pushes)
}
