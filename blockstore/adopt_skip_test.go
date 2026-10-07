package blockstore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBlockIdentity(t *testing.T) {
	ch := newFakeChain(5)
	n, hash, ok := BlockIdentity(fullBlock(t, ch, 5))
	require.True(t, ok)
	require.EqualValues(t, 5, n)
	require.Equal(t, normHash(hashOf(5, "a")), hash)

	upper := `{"number":"0x5","hash":"0x` + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789" + `"}`
	_, hash, ok = BlockIdentity(json.RawMessage(upper))
	require.True(t, ok)
	require.Equal(t, "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", hash)

	for _, bad := range []string{``, `null`, `[]`, `{}`, `{"number":"0x5"}`, `{"number":5,"hash":"0x00"}`,
		`{"number":"5","hash":"` + hashOf(5, "a") + `"}`, `{"number":"0x5","hash":"0x1234"}`, `{"number":"0x5","hash":null}`, `{"number":"0x5",`} {
		_, _, ok := BlockIdentity(json.RawMessage(bad))
		require.False(t, ok, bad)
	}
}

func TestAdoptBlockNeeded(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	block := fullBlock(t, ch, 20)
	header := headerOf(t, ch, 20)

	// Nothing held yet.
	require.True(t, c.AdoptBlockNeeded(block, true, true, false), "fresh upstream evidence is always adopted")
	require.True(t, c.AdoptBlockNeeded(block, true, true, true), "a cache replay may seed an empty height (restart)")
	require.False(t, c.AdoptBlockNeeded(block, true, false, false), "a by-hash result of an unheld block cannot adopt it")
	require.True(t, c.AdoptBlockNeeded(json.RawMessage(`{"garbage":1}`), true, false, true), "unknown identity takes the full parse")

	c.AdoptBlock(ctxb(), header, false, true, false)
	require.Equal(t, hashOf(20, "a"), c.CanonicalHash(20))

	// Held header, no body yet: a full result still has a body to adopt.
	require.True(t, c.AdoptBlockNeeded(block, true, true, true))
	require.True(t, c.AdoptBlockNeeded(block, true, false, false))
	// Held header, hashes-only result: nothing new.
	require.False(t, c.AdoptBlockNeeded(header, false, true, true), "cache replay of a held header")
	require.False(t, c.AdoptBlockNeeded(header, false, false, false), "by-hash header already held")

	c.AdoptBlock(ctxb(), block, true, true, false)
	_, ok := c.BlockByNumber(ctxb(), 20, true)
	require.True(t, ok)
	require.False(t, c.AdoptBlockNeeded(block, true, true, true), "cache replay of a held full block")
	require.False(t, c.AdoptBlockNeeded(block, true, false, false), "by-hash full block already held")
	require.True(t, c.AdoptBlockNeeded(block, true, true, false), "fresh upstream evidence reconfirms")

	// A different hash at a held height: weak or by-hash evidence never replaces it.
	ch.reorg(20, "b")
	other := fullBlock(t, ch, 20)
	require.False(t, c.AdoptBlockNeeded(other, true, true, true))
	require.False(t, c.AdoptBlockNeeded(other, true, false, false))
	require.True(t, c.AdoptBlockNeeded(other, true, true, false), "fresh conflicting evidence is a reorg")
}

// A held block observed again is not parsed: a cache replay is a no-op and a
// fresh upstream observation only reconfirms the held entry.
func TestAdoptBlock_HeldBlockSkipsParse(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	block := fullBlock(t, ch, 20)

	c.AdoptBlock(ctxb(), block, true, true, false)
	require.EqualValues(t, 1, c.adoptParses.Load(), "a new block is parsed and adopted")
	_, ok := c.BlockByNumber(ctxb(), 20, true)
	require.True(t, ok)

	c.AdoptBlock(ctxb(), block, true, true, true)
	c.AdoptBlock(ctxb(), headerOf(t, ch, 20), false, true, true)
	require.EqualValues(t, 1, c.adoptParses.Load(), "cache replay of a held height is not parsed")

	c.AdoptBlock(ctxb(), block, true, false, false)
	require.EqualValues(t, 1, c.adoptParses.Load(), "by-hash result of a held block is not parsed")

	// A fresh observation of the same hash reconfirms without a parse: once
	// the entry went stale, it serves again.
	base := time.Now()
	c.nowFn = func() time.Time { return base.Add(3 * time.Second) }
	_, ok = c.BlockByNumber(ctxb(), 20, false)
	require.False(t, ok, "stale without a fresh observation")
	c.AdoptBlock(ctxb(), block, true, true, true)
	_, ok = c.BlockByNumber(ctxb(), 20, false)
	require.False(t, ok, "a cache replay never confirms freshness")
	c.AdoptBlock(ctxb(), block, true, true, false)
	require.EqualValues(t, 1, c.adoptParses.Load(), "same hash reconfirmed without a parse")
	_, ok = c.BlockByNumber(ctxb(), 20, false)
	require.True(t, ok, "fresh observation reconfirmed the held entry")
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)

	// A held header with no body: the full block is parsed for its body.
	c.AdoptBlock(ctxb(), headerOf(t, ch, 19), false, true, false)
	require.EqualValues(t, 2, c.adoptParses.Load())
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 19), true, true, true)
	require.EqualValues(t, 3, c.adoptParses.Load(), "missing body of a held hash is still adopted")
	_, ok = c.BlockByNumber(ctxb(), 19, true)
	require.True(t, ok)
	_, body, _ = ch.counts()
	require.Zero(t, body)

	// A fresh conflicting hash is a reorg: parsed and adopted.
	ch.reorg(20, "b")
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 20), true, true, false)
	require.EqualValues(t, 4, c.adoptParses.Load())
	require.Equal(t, hashOf(20, "b"), c.CanonicalHash(20))
}

// BlockIdentity resolves a duplicated key like the full parse does (the last
// occurrence wins), so a held hash in a FIRST "hash" key cannot hide the
// effective, reorged hash from the reconfirm and adopt-skip paths.
func TestBlockIdentity_DuplicateKeysMatchFullParse(t *testing.T) {
	a, b := hashOf(5, "a"), hashOf(5, "b")
	for _, raw := range []string{
		`{"hash":"` + a + `","number":"0x4","hash":"` + b + `","number":"0x5"}`,
		`{"Hash":"` + a + `","NUMBER":"0x4","hash":"` + b + `","number":"0x5"}`,
	} {
		n, hash, ok := BlockIdentity(json.RawMessage(raw))
		require.True(t, ok, raw)
		sb, err := scanBlock(json.RawMessage(raw))
		require.NoError(t, err, raw)
		require.Equal(t, normHash(sb.b.Hash), hash, "identity agrees with the full decoder: "+raw)
		require.Equal(t, "0x5", sb.b.Number, raw)
		require.EqualValues(t, 5, n, raw)
		require.Equal(t, normHash(b), hash, raw)
	}
}

func TestAdoptBlock_DuplicateHashKeyIsNotReconfirmed(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	c.AdoptBlock(ctxb(), headerOf(t, ch, 20), false, true, false)
	require.Equal(t, hashOf(20, "a"), c.CanonicalHash(20))
	parses := c.adoptParses.Load()

	// The reorged block, prefixed with a duplicate "hash" key naming the held
	// (orphaned) hash. The effective hash is the last one: the reorged block.
	ch.reorg(20, "b")
	reorged := headerOf(t, ch, 20)
	dup := json.RawMessage(`{"hash":"` + hashOf(20, "a") + `",` + string(reorged[1:]))
	_, hash, ok := BlockIdentity(dup)
	require.True(t, ok)
	require.Equal(t, normHash(hashOf(20, "b")), hash)

	require.False(t, c.AdoptBlockNeeded(dup, false, true, true), "weak evidence never replaces a held hash")
	require.True(t, c.AdoptBlockNeeded(dup, false, true, false), "fresh conflicting evidence is a reorg")
	c.AdoptBlock(ctxb(), dup, false, true, false)
	require.Equal(t, parses+1, c.adoptParses.Load(), "the reorged block is parsed, not reconfirmed as the held one")
	require.Equal(t, hashOf(20, "b"), c.CanonicalHash(20), "reorg evidence is kept")
}
