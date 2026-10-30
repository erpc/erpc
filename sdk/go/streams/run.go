package streams

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// Bounds of the state Run keeps.
const (
	// chainKeep: refs of the applied chain kept to find fork points.
	chainKeep = 4096
	// branchKeep: blocks kept per endpoint, which a switch to it applies
	// (spec §9.5). An endpoint whose fork lies deeper restarts from the
	// merged cursor, and its server sends the Undo.
	branchKeep = 256
	// pendingKeep: transaction hashes kept to drop the second endpoint's
	// copies.
	pendingKeep = 10000
)

// Run streams cfg into h until ctx ends, then returns nil, or until a
// terminal end (spec §9.5): an authentication, permission or option error
// from the server, *MessageTooLargeError, or an error h returned, at once;
// *OutOfRangeError or *FinalRevertedError from an endpoint while no other
// endpoint streams.
// Every other end reconnects from the cursor: at once after a stream that
// flowed ended UNAVAILABLE or went silent for 3 heartbeats, else after a
// jittered backoff. With two endpoints both stream from the merged cursor
// and Run follows the one with the highest block. No handler call starts
// after ctx ends.
func Run(ctx context.Context, cfg Config, h Handler) error {
	p, err := cfg.plan(h)
	if err != nil {
		return err
	}
	r := &runner{p: p, h: h, cursor: cfg.Cursor, events: make(chan event, 16), leader: -1,
		branches: make([]branch, len(p.eps)), seen: map[Hash]struct{}{}}
	if cfg.Cursor != nil {
		tok, err := cfg.Cursor.Load()
		if err != nil {
			return fmt.Errorf("streams: load cursor: %w", err)
		}
		if tok != "" {
			ref, err := ParseToken(tok)
			if err != nil {
				return fmt.Errorf("streams: stored cursor: %w", err)
			}
			r.resume.Store(&ref)
			r.chain = []BlockRef{ref}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for i, ep := range p.eps {
		l := &link{idx: i, ep: ep}
		r.links = append(r.links, l)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runLink(ctx, l)
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-r.events:
			if ctx.Err() != nil {
				return nil
			}
			if err := r.handle(ctx, ev); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// runner is one Run. Its loop alone touches the fields below resume.
type runner struct {
	p      *plan
	h      Handler
	cursor Cursor
	events chan event
	links  []*link
	// resume is the cursor new sessions resume from: the applied chain's
	// top.
	resume atomic.Pointer[BlockRef]

	// chain is the applied chain, oldest first, each the parent of the
	// next; empty until the first block.
	chain    []BlockRef
	branches []branch
	leader   int
	seen     map[Hash]struct{}
	seenRing []Hash
}

func (r *runner) emit(ctx context.Context, ev event) bool {
	select {
	case r.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *runner) status(s Status) {
	if r.h.Status != nil {
		r.h.Status(s)
	}
}

func (r *runner) handle(ctx context.Context, ev event) error {
	b := &r.branches[ev.link]
	switch {
	case ev.kind == evOpen:
		b.reset(ev.sess, ev.from)
		return nil
	case ev.sess != b.sess:
		return nil
	case ev.kind == evEnd:
		b.live = false
		if ev.err != nil && (!ev.perEndpoint || !r.streaming()) {
			return ev.err
		}
		r.status(Status{State: StateReconnecting, Reason: ev.reason, Endpoint: r.links[ev.link].ep.raw})
		return r.follow(ctx)
	case !b.live:
		return nil
	case ev.kind == evStatus:
		r.status(ev.status)
		return nil
	case r.p.kind == NewPendingTransactions:
		return r.applyPending(ctx, ev.block)
	case ev.kind == evUndo:
		b.undo(ev.ref)
	default:
		if !b.add(ev.block) {
			r.restart(ev.link)
		}
	}
	return r.follow(ctx)
}

// restart drops endpoint i's branch and resumes its stream from the
// applied chain's top after the reconnect backoff.
func (r *runner) restart(i int) {
	b := &r.branches[i]
	b.live = false
	r.links[i].restart(b.sess)
}

// streaming: some endpoint's stream is open.
func (r *runner) streaming() bool {
	for i := range r.branches {
		if r.branches[i].live {
			return true
		}
	}
	return false
}

// follow applies the leader's branch: the endpoint with the highest block,
// the current one on a tie, else the next ready one. A branch with no
// block in common with the applied chain restarts from its top. An Undo
// counts only in the follow it triggers: a branch that does not lead then
// waits for its next block.
func (r *runner) follow(ctx context.Context) error {
	defer func() {
		for i := range r.branches {
			r.branches[i].undone = false
		}
	}()
	var order []int
	if r.leader >= 0 && r.branches[r.leader].ready() {
		order = append(order, r.leader)
	}
	for i := range r.branches {
		if i == r.leader || !r.branches[i].ready() {
			continue
		}
		if len(order) > 0 && r.branches[i].high > r.branches[order[0]].high {
			order = slices.Insert(order, 0, i)
		} else {
			order = append(order, i)
		}
	}
	for _, i := range order {
		ok, err := r.followBranch(ctx, i)
		if err != nil {
			return err
		}
		if ok {
			r.leader = i
			return nil
		}
		r.restart(i)
	}
	return nil
}

// followBranch moves the applied chain onto branch i: Undo to the fork
// point when the chain holds blocks above it, then Apply each block of the
// branch above it. A block the chain holds is never applied again. False
// when the branch shares no block with the chain.
func (r *runner) followBranch(ctx context.Context, i int) (bool, error) {
	b := &r.branches[i]
	k := len(b.blocks) - 1
	for ; k >= 0 && !r.has(b.blocks[k].BlockRef); k-- {
	}
	var fork BlockRef
	switch {
	case k >= 0:
		fork = b.blocks[k].BlockRef
	case len(r.chain) == 0:
		fork = *b.base
		r.chain = []BlockRef{fork}
	case r.has(*b.base):
		fork = *b.base
	case b.trusted && b.base.Number <= r.chain[0].Number:
		// Below every block the chain holds: the server's Undo, or resend,
		// to a block this client applied before.
		fork = *b.base
	default:
		return false, nil
	}
	above := b.blocks[k+1:]
	if len(above) == 0 && !b.undone {
		return true, nil
	}
	b.undone = false
	if r.chain[len(r.chain)-1] != fork {
		if r.h.Undo == nil {
			return false, &FinalRevertedError{Endpoint: r.links[i].ep.raw,
				Message: fmt.Sprintf("final_reverted: blocks above %d were reverted", fork.Number)}
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := r.h.Undo(ctx, fork); err != nil {
			return false, err
		}
		if r.has(fork) {
			r.chain = r.chain[:fork.Number-r.chain[0].Number+1]
		} else {
			r.chain = []BlockRef{fork}
		}
		if err := r.save(fork); err != nil {
			return false, err
		}
	}
	for _, blk := range above {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := r.h.Apply(ctx, blk); err != nil {
			return false, err
		}
		r.chain = append(r.chain, blk.BlockRef)
		if len(r.chain) > 2*chainKeep {
			r.chain = slices.Clone(r.chain[len(r.chain)-chainKeep:])
		}
		if err := r.save(blk.BlockRef); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *runner) has(ref BlockRef) bool {
	if len(r.chain) == 0 || ref.Number < r.chain[0].Number {
		return false
	}
	i := ref.Number - r.chain[0].Number
	return i < uint64(len(r.chain)) && r.chain[i] == ref
}

func (r *runner) save(ref BlockRef) error {
	r.resume.Store(&ref)
	if r.cursor == nil {
		return nil
	}
	if err := r.cursor.Save(ref.Token()); err != nil {
		return fmt.Errorf("streams: save cursor: %w", err)
	}
	return nil
}

// applyPending applies a batch without the transactions another endpoint
// delivered before.
func (r *runner) applyPending(ctx context.Context, b Block) error {
	fresh := func(h Hash) bool {
		if _, ok := r.seen[h]; ok {
			return false
		}
		if len(r.seenRing) == pendingKeep {
			delete(r.seen, r.seenRing[0])
			r.seenRing = r.seenRing[1:]
		}
		r.seen[h] = struct{}{}
		r.seenRing = append(r.seenRing, h)
		return true
	}
	out := Block{}
	for _, h := range b.Hashes {
		if fresh(h) {
			out.Hashes = append(out.Hashes, h)
		}
	}
	for _, tx := range b.Transactions {
		h, err := hashOf(tx.GetHash())
		if err != nil {
			return errors.Join(errors.New("streams: pending transaction"), err)
		}
		if fresh(h) {
			out.Transactions = append(out.Transactions, tx)
		}
	}
	if len(out.Hashes) == 0 && len(out.Transactions) == 0 || ctx.Err() != nil {
		return nil
	}
	return r.h.Apply(ctx, out)
}

// branch is what one endpoint's current session delivered: blocks linked
// from base, the block its stream resumed from or the parent of its first.
type branch struct {
	sess   uint64
	live   bool
	base   *BlockRef
	blocks []Block
	// high is the highest block number the session delivered: an Undo
	// does not lower it, so the endpoint keeps the lead while it resends.
	high uint64
	// undone: the session's last word was an Undo, which drops the chain
	// above the branch's top even before the new branch arrives; cleared
	// by the follow it triggers.
	undone bool
	// trusted: base is a fork point the server named, by Undo or resend.
	trusted bool
}

func (b *branch) ready() bool { return b.live && b.base != nil }

func (b *branch) reset(sess uint64, from *BlockRef) {
	*b = branch{sess: sess, live: true}
	if from != nil {
		f := *from
		b.base, b.high = &f, f.Number
	}
}

// add appends blk, dropping the blocks at and above its number: a
// resend after a reorg names its new parent. False when blk links to
// nothing the branch holds.
func (b *branch) add(blk Block) bool {
	if blk.Number == 0 {
		return false
	}
	parent := BlockRef{Number: blk.Number - 1, Hash: blk.ParentHash}
	switch {
	case b.base == nil:
		b.base, b.blocks = &parent, []Block{blk}
	case parent.Number < b.base.Number:
		// A resend from below the resume cursor: an undo to the parent.
		b.base, b.blocks, b.trusted = &parent, []Block{blk}, true
	case parent == *b.base:
		b.blocks = append(b.blocks[:0], blk)
	case parent.Number == b.base.Number:
		return false
	default:
		i := parent.Number - b.base.Number - 1
		if i >= uint64(len(b.blocks)) || b.blocks[i].BlockRef != parent {
			return false
		}
		b.blocks = append(b.blocks[:i+1], blk)
	}
	b.high, b.undone = max(b.high, blk.Number), false
	if len(b.blocks) > branchKeep {
		cut := len(b.blocks) - branchKeep
		base := b.blocks[cut-1].BlockRef
		b.base, b.blocks, b.trusted = &base, slices.Clone(b.blocks[cut:]), false
	}
	return true
}

// undo drops the blocks above fp.
func (b *branch) undo(fp BlockRef) {
	b.undone = true
	if b.base != nil && fp.Number > b.base.Number {
		i := fp.Number - b.base.Number - 1
		if i < uint64(len(b.blocks)) && b.blocks[i].BlockRef == fp {
			b.blocks = b.blocks[:i+1]
			return
		}
	}
	if b.base != nil && *b.base == fp {
		b.blocks = nil
		return
	}
	b.base, b.blocks, b.trusted = &fp, nil, true
}
