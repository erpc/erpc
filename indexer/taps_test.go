package indexer

import "testing"

// A batch continues a tap by hash, the number rising: by more than one
// only over null rounds, where the parentHash still names the block below
// (spec §6.5, §15.3).
func TestLinked(t *testing.T) {
	h := func(b byte) [32]byte { return [32]byte{b} }
	blk := func(n uint64, hash, parent byte) Block {
		return Block{Number: n, Hash: h(hash), ParentHash: h(parent)}
	}
	at10 := Cursor{Number: 10, Hash: h(0xa)}
	cases := []struct {
		name string
		b    Batch
		want bool
	}{
		{"the next block", Batch{Applied: []Block{blk(11, 0xb, 0xa)}}, true},
		{"a jump over a null round, linked by hash", Batch{Applied: []Block{blk(12, 0xc, 0xa)}}, true},
		{"a jump over two null rounds, then the next block", Batch{Applied: []Block{blk(13, 0xd, 0xa), blk(14, 0xe, 0xd)}}, true},
		{"a jump whose parentHash is not the cursor", Batch{Applied: []Block{blk(12, 0xc, 0xf)}}, false},
		{"the next height, another parent", Batch{Applied: []Block{blk(11, 0xb, 0xf)}}, false},
		{"a block at the cursor's height", Batch{Applied: []Block{blk(10, 0xb, 0xa)}}, false},
		{"a later block that does not link", Batch{Applied: []Block{blk(11, 0xb, 0xa), blk(13, 0xd, 0xf)}}, false},
		{"a revert, then a branch over a null round", Batch{Reverted: []Block{blk(10, 0xa, 0x9)}, Applied: []Block{blk(11, 0x1, 0x9), blk(13, 0x3, 0x1)}}, true},
		{"a revert from another block", Batch{Reverted: []Block{blk(10, 0xf, 0x9)}, Applied: []Block{blk(10, 0x1, 0x9)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := linked(at10, tc.b); got != tc.want {
				t.Fatalf("linked = %v, want %v", got, tc.want)
			}
		})
	}
}
