package chainsim

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

const (
	// HistoryBlocks are minted at start, one block time apart, ending now.
	HistoryBlocks = 64
	firstNumber   = 0x1000
	// FinalizedLag is how far below its head a vendor's finalized block is.
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
}

// Chain is one simulated chain: its canonical branch, every block ever
// minted, and the vendors serving it. One lock guards the chain and every
// vendor's view and faults.
type Chain struct {
	cfg Config

	mu      sync.Mutex
	rng     *rand.Rand
	minted  uint64
	canon   []*Block
	byHash  map[Hash]*Block
	halted  bool
	vendors []*Vendor
	forks   map[*Fork]bool

	stop chan struct{}
	done chan struct{}
}

// Fork is a branch some vendors serve instead of the canonical chain; it
// gains a block every tick.
type Fork struct{ tip *Block }

// NewChain mints the history; Start makes blocks.
func NewChain(cfg Config) *Chain {
	c := &Chain{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, uint64(cfg.ChainID))), byHash: make(map[Hash]*Block),
		forks: make(map[*Fork]bool), stop: make(chan struct{}), done: make(chan struct{})}
	start := time.Now().Add(-HistoryBlocks * cfg.BlockTime)
	var parent *Block
	for i := range HistoryBlocks {
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

// Start makes a block every block time until Close.
func (c *Chain) Start() {
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

// Close stops block production and every vendor.
func (c *Chain) Close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
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
	var k kind
	switch r := c.rng.IntN(8); {
	case r == 0:
		k = kindEmpty
	case r <= 2:
		k = kindQuiet
	default:
		k = kindMatching
	}
	txs := content(c.rng, k)
	tag := fmt.Sprintf("%d/%d", c.cfg.Seed, c.minted)
	hash, raw := header(c.cfg.ChainID, parent, number, ts, tag, txs, c.cfg.HeaderPad)
	b := &Block{Number: number, Hash: hash, Parent: parent, Time: ts, MintedAt: time.Now(), minority: minority, tag: tag, txs: txs, raw: raw}
	c.byHash[hash] = b
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

// Halt stops or resumes canonical block production.
func (c *Chain) Halt(halted bool) {
	c.mu.Lock()
	c.halted = halted
	c.mu.Unlock()
}

// tick adds a canonical block unless the chain halted, grows every fork,
// and lets each vendor follow; wss vendors first push the new block's
// transactions as pending ones.
func (c *Chain) tick() {
	c.mu.Lock()
	now := time.Now()
	var pushes []push
	if !c.halted {
		b := c.mintLocked(c.tipLocked(), c.tipLocked().Number+1, now, false)
		c.canon = append(c.canon, b)
		for _, v := range c.vendors {
			pushes = v.pendingPushesLocked(b, pushes)
		}
	}
	for f := range c.forks {
		f.tip = c.mintLocked(f.tip, f.tip.Number+1, now, true)
	}
	c.mu.Unlock()
	writePushes(pushes)
	c.refresh()
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

// StartFork moves vs onto a minority branch off the canonical head's
// parent; it grows every tick until EndFork.
func (c *Chain) StartFork(vs ...*Vendor) *Fork {
	c.mu.Lock()
	tip := c.tipLocked()
	f := &Fork{tip: c.mintLocked(tip.Parent, tip.Number, time.Now(), true)}
	c.forks[f] = true
	for _, v := range vs {
		v.fork = f
	}
	c.mu.Unlock()
	c.refresh()
	return f
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
