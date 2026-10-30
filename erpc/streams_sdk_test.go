package erpc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/sdk/go/streams"
	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Go streams library (streams spec §9.4, §9.5) against in-process eRPC
// instances on the simulated chain. Each endpoint goes through an
// sdkRelay, which keeps the library's endpoint while an instance restarts
// on a new port, and can black-hole the link with its sockets kept.

// sdkRelay forwards TCP connections to the current target.
type sdkRelay struct {
	ln     net.Listener
	target atomic.Pointer[string]
	held   atomic.Bool
}

func startSDKRelay(t *testing.T, target string) *sdkRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &sdkRelay{ln: ln}
	r.retarget(target)
	t.Cleanup(func() { _ = ln.Close(); r.held.Store(false) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.pipe(c)
		}
	}()
	return r
}

func (r *sdkRelay) endpoint() string { return "grpc://" + r.ln.Addr().String() + "/test_ws/evm/123" }

func (r *sdkRelay) retarget(addr string) { r.target.Store(&addr) }

// hold black-holes every connection, open and new, until released: bytes
// read are kept back, sockets stay open.
func (r *sdkRelay) hold(on bool) { r.held.Store(on) }

func (r *sdkRelay) pipe(c net.Conn) {
	s, err := net.DialTimeout("tcp", *r.target.Load(), 2*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			for r.held.Load() {
				time.Sleep(10 * time.Millisecond)
			}
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go cp(s, c)
	go cp(c, s)
	<-done
	_ = c.Close()
	_ = s.Close()
}

type sdkStatus struct {
	at time.Time
	s  streams.Status
}

// sdkApp is an application on the library: it holds the heads and logs of
// the branch it applied, and records each break of the library's
// guarantees: a block not linked to the last one, a block applied twice
// (simulated reorgs never bring a branch back, so a block is applied once
// in all), an Undo at or above the last block.
type sdkApp struct {
	mu       sync.Mutex
	first    uint64
	top      *streams.BlockRef
	seen     map[streams.Hash]bool
	heads    map[uint64]streams.Hash
	held     map[chainsim.LogKey]uint64
	applied  []time.Time
	undos    []streams.BlockRef
	statuses []sdkStatus
	problems []string
}

func newSDKApp() *sdkApp {
	return &sdkApp{seen: map[streams.Hash]bool{}, heads: map[uint64]streams.Hash{}, held: map[chainsim.LogKey]uint64{}}
}

func (a *sdkApp) handler() streams.Handler {
	return streams.Handler{
		Apply: func(_ context.Context, b streams.Block) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.top != nil && (b.Number != a.top.Number+1 || b.ParentHash != a.top.Hash) {
				a.problems = append(a.problems, fmt.Sprintf("block %d %s parent %s applied on %d %s", b.Number, b.Hash, b.ParentHash, a.top.Number, a.top.Hash))
			}
			if a.seen[b.Hash] {
				a.problems = append(a.problems, fmt.Sprintf("block %d %s applied twice", b.Number, b.Hash))
			}
			if a.first == 0 {
				a.first = b.Number
			}
			a.seen[b.Hash] = true
			a.heads[b.Number] = b.Hash
			for _, l := range b.Logs {
				a.held[chainsim.LogKey{BlockHash: gethcommon.BytesToHash(l.BlockHash), LogIndex: int(l.LogIndex)}] = b.Number
			}
			ref := b.BlockRef
			a.top = &ref
			a.applied = append(a.applied, time.Now())
			return nil
		},
		Undo: func(_ context.Context, fp streams.BlockRef) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.top != nil && fp.Number >= a.top.Number {
				a.problems = append(a.problems, fmt.Sprintf("undo to %d at %d", fp.Number, a.top.Number))
			}
			for n := range a.heads {
				if n > fp.Number {
					delete(a.heads, n)
				}
			}
			for k, n := range a.held {
				if n > fp.Number {
					delete(a.held, k)
				}
			}
			a.top = &fp
			a.undos = append(a.undos, fp)
			return nil
		},
		Status: func(s streams.Status) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.statuses = append(a.statuses, sdkStatus{time.Now(), s})
		},
	}
}

func (a *sdkApp) appliedSince(t time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, at := range a.applied {
		if !at.Before(t) {
			n++
		}
	}
	return n
}

func (a *sdkApp) undoCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.undos)
}

// statusSince is the first status of state after t.
func (a *sdkApp) statusSince(t time.Time, state streams.State) (sdkStatus, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, st := range a.statuses {
		if st.s.State == state && !st.at.Before(t) {
			return st, true
		}
	}
	return sdkStatus{}, false
}

// waitTip waits until the app's last block is the canonical tip.
func (a *sdkApp) waitTip(t *testing.T, s *chaosSeed) {
	t.Helper()
	require.Eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.top != nil && a.top.Hash == streams.Hash(s.chain.Tip().Hash)
	}, 30*time.Second, 20*time.Millisecond, "the app reaches the canonical tip")
}

// requireCanonical: no guarantee broke, and the app holds the canonical
// chain's heads and, for filter ≥ 0, its logs of that filter, from its
// first block to its last.
func (a *sdkApp) requireCanonical(t *testing.T, s *chaosSeed, filter int) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Empty(t, a.problems)
	require.NotNil(t, a.top)
	last := a.top.Number
	require.Greater(t, last, a.first+20, "a flowing stream")
	for n := a.first; n <= last; n++ {
		require.Equal(t, streams.Hash(s.chain.CanonAt(n).Hash), a.heads[n], "block %d canonical", n)
	}
	if filter < 0 {
		return
	}
	want := s.chain.CanonicalLogs(chainsim.Filters[filter], a.first, last)
	for k, n := range a.held {
		_, ok := want[k]
		assert.True(t, ok, "holds log %d of block %d, not canonical", k.LogIndex, n)
		delete(want, k)
	}
	assert.Empty(t, want, "misses canonical logs of blocks %d-%d", a.first, last)
}

func sdkFilter(i int) streams.Filter {
	f := chainsim.Filters[i]
	if f.Address != nil {
		return streams.Filter{Address: []string{f.Address.Hex()}}
	}
	return streams.Filter{Topics: [][]string{{f.Topic0.Hex()}}}
}

// runSDK runs the library until the test stops it; stop returns Run's
// error.
func runSDK(t *testing.T, cfg streams.Config, a *sdkApp) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- streams.Run(ctx, cfg, a.handler()) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				err = fmt.Errorf("Run did not return 10s after its context ended")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// The library resumes from its file cursor across an instance restart and
// reorgs: every block once, linked, logs canonical, each reorg an Undo;
// the cursor file names the last block. A cursor older than the window
// ends Run with OutOfRangeError naming it and the oldest block, and
// applies nothing.
func TestStreamsSDK_ResumeAcrossRestartAndUndo(t *testing.T) {
	s := startFinalitySeed(t, 241, chainsim.Faults{})
	relay := startSDKRelay(t, s.startSecond().addr)
	cursor := filepath.Join(t.TempDir(), "erpc.cursor")
	app := newSDKApp()
	stop := runSDK(t, streams.Config{Endpoints: []string{relay.endpoint()}, Kind: streams.Logs, Filter: sdkFilter(0),
		Cursor: streams.FileCursor(cursor), HeartbeatSeconds: 1}, app)
	require.Eventually(t, func() bool { return app.appliedSince(time.Time{}) >= 8 }, 20*time.Second, 20*time.Millisecond, "logs flow")
	app.waitTip(t, s)
	s.reorg(3)
	s.blocks(8)
	require.Eventually(t, func() bool { return app.undoCount() >= 1 }, 10*time.Second, 20*time.Millisecond, "the reorg reaches the app as an Undo")

	// The instance stops while 4 blocks are minted; the chain halts while
	// a new instance boots on another port, so the cursor stays inside
	// its window.
	stopped := time.Now()
	s.stopGrpc(1)
	s.blocks(4)
	s.chain.Halt(true)
	relay.retarget(s.startSecond().addr)
	s.chain.Halt(false)
	s.blocks(5)
	app.waitTip(t, s)
	s.reorg(2)
	s.blocks(10)
	app.waitTip(t, s)
	require.NoError(t, stop())
	app.requireCanonical(t, s, 0)
	_, ok := app.statusSince(stopped, streams.StateReconnecting)
	assert.True(t, ok, "the restart is reported as reconnecting")
	assert.GreaterOrEqual(t, app.undoCount(), 2, "an Undo per reorg")
	saved, err := os.ReadFile(cursor)
	require.NoError(t, err)
	assert.Equal(t, app.top.Token(), strings.TrimSpace(string(saved)), "the cursor file names the last block")

	tip := s.chain.Tip()
	var old *chainsim.Block
	for b := tip; b != nil && old == nil; b = b.Parent {
		if b.Time+uint64(s.maxBackfill()/time.Second)+2 < tip.Time && b.Number+uint64(chaosReorgWindow) < tip.Number {
			old = b
		}
	}
	require.NotNil(t, old, "the chain holds a block older than the window")
	oldRef := streams.BlockRef{Number: old.Number, Hash: streams.Hash(old.Hash)}
	stale := filepath.Join(t.TempDir(), "stale.cursor")
	require.NoError(t, os.WriteFile(stale, []byte(oldRef.Token()), 0o600))
	late := newSDKApp()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = streams.Run(ctx, streams.Config{Endpoints: []string{relay.endpoint()}, Kind: streams.Logs, Filter: sdkFilter(0),
		Cursor: streams.FileCursor(stale), HeartbeatSeconds: 1}, late.handler())
	var oor *streams.OutOfRangeError
	require.ErrorAs(t, err, &oor)
	assert.Equal(t, oldRef.Token(), oor.Cursor)
	assert.Equal(t, "below_window", oor.Reason)
	require.NotNil(t, oor.Oldest, "the end names the oldest resumable block")
	assert.Greater(t, oor.Oldest.Number, old.Number)
	assert.Zero(t, late.appliedSince(time.Time{}), "nothing applied past the gap")
}

// A link black-holed with its sockets kept: the watchdog ends the silent
// stream within 3 heartbeats, reported reconnecting with reason watchdog;
// once the link returns the stream resumes from the cursor, and a reorg
// of heads, resent from the fork point, reaches the app as an Undo. The
// app's heads are canonical, every block once.
func TestStreamsSDK_WatchdogReconnects(t *testing.T) {
	s := startFinalitySeed(t, 251, chainsim.Faults{})
	relay := startSDKRelay(t, s.startGrpc(0, s.srv, nil, s.nw.Load()).addr)
	app := newSDKApp()
	stop := runSDK(t, streams.Config{Endpoints: []string{relay.endpoint()}, Kind: streams.NewHeads, HeartbeatSeconds: 1}, app)
	require.Eventually(t, func() bool { return app.appliedSince(time.Time{}) >= 5 }, 20*time.Second, 20*time.Millisecond, "heads flow")

	heldAt := time.Now()
	relay.hold(true)
	var st sdkStatus
	require.Eventually(t, func() bool {
		var ok bool
		st, ok = app.statusSince(heldAt, streams.StateReconnecting)
		return ok
	}, 10*time.Second, 20*time.Millisecond, "the watchdog ends the silent stream")
	relay.hold(false)
	released := time.Now()
	assert.Equal(t, "watchdog", st.s.Reason)
	assert.LessOrEqual(t, st.at.Sub(heldAt), 3*time.Second+time.Second, "within 3 heartbeats of 1s")
	require.Eventually(t, func() bool { return app.appliedSince(released) > 0 }, 10*time.Second, 20*time.Millisecond, "heads flow again")

	app.waitTip(t, s)
	s.reorg(2)
	s.blocks(10)
	app.waitTip(t, s)
	require.NoError(t, stop())
	app.requireCanonical(t, s, -1)
	assert.GreaterOrEqual(t, app.undoCount(), 1, "the reorg's resend is an Undo")
}

// Two endpoints merged (spec §9.5): each instance's server stops in turn
// and comes back on a new port, under reorgs. The app keeps applying from
// the other within the watchdog, holds the canonical logs, every block
// once per branch, every reorg an Undo.
func TestStreamsSDK_TwoEndpointMerge(t *testing.T) {
	s := startFinalitySeed(t, 257, chainsim.Faults{})
	r0 := startSDKRelay(t, s.startGrpc(0, s.srv, nil, s.nw.Load()).addr)
	r1 := startSDKRelay(t, s.startSecond().addr)
	app := newSDKApp()
	stop := runSDK(t, streams.Config{Endpoints: []string{r0.endpoint(), r1.endpoint()}, Kind: streams.Logs, Filter: sdkFilter(1),
		HeartbeatSeconds: 1}, app)
	require.Eventually(t, func() bool { return app.appliedSince(time.Time{}) >= 8 }, 20*time.Second, 20*time.Millisecond, "logs flow")
	app.waitTip(t, s)
	s.reorg(2)
	s.blocks(8)

	watchdog := 3 * time.Second
	killed := time.Now()
	s.stopGrpc(1)
	require.Eventually(t, func() bool { return app.appliedSince(killed) > 0 }, watchdog, 10*time.Millisecond, "instance 1 serves on")
	s.blocks(5)
	app.waitTip(t, s)
	s.reorg(3)
	s.blocks(5)
	r1.retarget(s.startSecond().addr)
	s.blocks(10)

	killed = time.Now()
	s.stopGrpc(0)
	require.Eventually(t, func() bool { return app.appliedSince(killed) > 0 }, watchdog, 10*time.Millisecond, "instance 2 serves on")
	r0.retarget(s.startGrpc(0, s.srv, nil, s.nw.Load()).addr)
	s.blocks(10)
	app.waitTip(t, s)
	s.reorg(2)
	s.blocks(10)
	app.waitTip(t, s)
	require.NoError(t, stop())
	app.requireCanonical(t, s, 1)
	assert.GreaterOrEqual(t, app.undoCount(), 2, "the reorgs reach the app as Undos")
}
