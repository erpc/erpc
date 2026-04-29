package indexer

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCanonicalChain_NormalExtension(t *testing.T) {
	c := newCanonicalChain(8)
	ev := c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	if len(ev) != 0 {
		t.Fatalf("first head, expected no evictions, got %d", len(ev))
	}
	ev = c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})
	if len(ev) != 0 {
		t.Fatalf("clean extension, expected no evictions, got %v", ev)
	}
	if tip := c.head(); tip.Number != 101 || tip.Hash != "0xB" {
		t.Fatalf("tip should be 101/0xB, got %+v", tip)
	}
}

func TestCanonicalChain_OneBlockReorg(t *testing.T) {
	c := newCanonicalChain(8)
	c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})

	// A new head at 101 with a different hash than 0xB is a 1-block
	// reorg. parentHash 0xA matches ring[0]; ring[1]=0xB evicted.
	evicted := c.observeHead(BlockRef{Number: 101, Hash: "0xC", ParentHash: "0xA"})
	if len(evicted) != 1 || evicted[0].Hash != "0xB" {
		t.Fatalf("expected 1 eviction of 0xB, got %+v", evicted)
	}
	if tip := c.head(); tip.Hash != "0xC" {
		t.Fatalf("tip should be 0xC, got %+v", tip)
	}
}

func TestCanonicalChain_DeepReorgToCommonAncestor(t *testing.T) {
	c := newCanonicalChain(8)
	c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})
	c.observeHead(BlockRef{Number: 102, Hash: "0xC", ParentHash: "0xB"})
	c.observeHead(BlockRef{Number: 103, Hash: "0xD", ParentHash: "0xC"})

	// Incoming head at 102 claims parentHash=0xA — reverts back two
	// blocks. Expect ring[0xB, 0xC, 0xD] to be evicted.
	evicted := c.observeHead(BlockRef{Number: 101, Hash: "0xX", ParentHash: "0xA"})
	if len(evicted) != 3 {
		t.Fatalf("expected 3 evictions, got %d (%+v)", len(evicted), evicted)
	}
	hashes := []string{evicted[0].Hash, evicted[1].Hash, evicted[2].Hash}
	if !reflect.DeepEqual(hashes, []string{"0xB", "0xC", "0xD"}) {
		t.Fatalf("expected [0xB 0xC 0xD], got %v", hashes)
	}
}

func TestCanonicalChain_UnknownParentFallback(t *testing.T) {
	c := newCanonicalChain(8)
	c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})

	// Incoming head at 102 with unknown parent. Fallback: evict anything
	// with num >= 102 (none), push. No evictions.
	ev := c.observeHead(BlockRef{Number: 102, Hash: "0xC", ParentHash: "0xUnknown"})
	if len(ev) != 0 {
		t.Fatalf("expected 0 evictions (gap case), got %+v", ev)
	}
	// Incoming head at 101 with unknown parent: same-height reorg we
	// can't fully reason about. Evict 0xB, push 0xD. The ring state is
	// best-effort after this kind of event.
	c = newCanonicalChain(8)
	c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})
	ev = c.observeHead(BlockRef{Number: 101, Hash: "0xD", ParentHash: "0xUnknown"})
	if len(ev) != 1 || ev[0].Hash != "0xB" {
		t.Fatalf("expected 1 eviction (0xB) on unknown-parent same-height, got %+v", ev)
	}
}

func TestCanonicalChain_StaleHeadIgnored(t *testing.T) {
	c := newCanonicalChain(8)
	c.observeHead(BlockRef{Number: 100, Hash: "0xA", ParentHash: "0xZ"})
	c.observeHead(BlockRef{Number: 101, Hash: "0xB", ParentHash: "0xA"})

	// Stale head at 99 — ignore.
	if ev := c.observeHead(BlockRef{Number: 99, Hash: "0xOld", ParentHash: "0xOlder"}); len(ev) != 0 {
		t.Fatalf("stale head must produce 0 evictions, got %+v", ev)
	}
	if tip := c.head(); tip.Number != 101 {
		t.Fatalf("tip must still be 101 after stale head, got %+v", tip)
	}
}

func TestCanonicalChain_RingCapacityEvictsOldest(t *testing.T) {
	c := newCanonicalChain(3)
	for i := int64(100); i <= 104; i++ {
		c.observeHead(BlockRef{Number: i, Hash: stamp(i), ParentHash: stamp(i - 1)})
	}
	if tip := c.head(); tip.Number != 104 {
		t.Fatalf("tip should be 104, got %+v", tip)
	}
	// Ring should hold the last 3: 102, 103, 104.
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ring) != 3 {
		t.Fatalf("ring size should be 3, got %d", len(c.ring))
	}
	if c.ring[0].Number != 102 {
		t.Fatalf("oldest should be 102, got %+v", c.ring[0])
	}
}

func TestCanonicalChain_LogIndexDrain(t *testing.T) {
	c := newCanonicalChain(8)
	// Seed 3 logs against block hash 0xB.
	entry := loggedLog{filterHash: "h1", block: BlockRef{Number: 101, Hash: "0xB"}, payload: json.RawMessage(`{}`)}
	c.indexLog(entry)
	c.indexLog(entry)
	c.indexLog(entry)

	drained := c.drainLogsFor("0xB")
	if len(drained) != 3 {
		t.Fatalf("want 3 drained, got %d", len(drained))
	}
	// Second drain returns nothing.
	if got := c.drainLogsFor("0xB"); len(got) != 0 {
		t.Fatalf("drain should be exhaustive, got %d", len(got))
	}
}

func stamp(n int64) string {
	// Simple deterministic hash-like label for tests.
	return "0x" + string(rune('A'+n%26))
}
