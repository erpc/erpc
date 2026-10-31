package streams_test

// The library against real eRPC pods over a simulated chain
// (test/streamsim), as the Node library's tests run it. ERPC_BIN names a
// built cmd/erpc; else it is built.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/sdk/go/streams"
)

type simRef struct {
	Number     uint64   `json:"number"`
	Hash       string   `json:"hash"`
	ParentHash string   `json:"parentHash"`
	Keys       []string `json:"keys"`
}

type simReply struct {
	Error  string   `json:"error"`
	Tip    simRef   `json:"tip"`
	Blocks []simRef `json:"blocks"`
}

type sim struct {
	in        io.WriteCloser
	out       *bufio.Scanner
	endpoints []string
	filters   []map[string]interface{}
}

// startSim builds eRPC and test/streamsim and runs 2 pods.
func startSim(t *testing.T) *sim {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("go build %s: %v", pkg, err)
		}
	}
	bin := os.Getenv("ERPC_BIN")
	if bin == "" {
		bin = filepath.Join(dir, "erpc")
		build(bin, "./cmd/erpc")
	}
	build(filepath.Join(dir, "streamsim"), "./test/streamsim")
	cmd := exec.Command(filepath.Join(dir, "streamsim"), "-erpc", bin, "-dir", dir, "-pods", "2")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Wait()
	})
	s := &sim{in: in, out: bufio.NewScanner(out)}
	s.out.Buffer(make([]byte, 1<<20), 16<<20)
	if !s.out.Scan() {
		t.Fatal("streamsim exited before ready")
	}
	var ready struct {
		Endpoints []string                 `json:"endpoints"`
		Filters   []map[string]interface{} `json:"filters"`
	}
	if err := json.Unmarshal(s.out.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	s.endpoints, s.filters = ready.Endpoints, ready.Filters
	// Pods serve once their trackers follow the chain.
	s.send(t, map[string]interface{}{"op": "wait", "blocks": 12})
	return s
}

// send runs one streamsim command and returns its reply.
func (s *sim) send(t *testing.T, cmd map[string]interface{}) simReply {
	t.Helper()
	line, _ := json.Marshal(cmd)
	if _, err := fmt.Fprintf(s.in, "%s\n", line); err != nil {
		t.Fatal(err)
	}
	if !s.out.Scan() {
		t.Fatalf("streamsim exited on %s", line)
	}
	var r simReply
	if err := json.Unmarshal(s.out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Error != "" {
		t.Fatalf("%s: %s", line, r.Error)
	}
	return r
}

func (s *sim) filter(i int) streams.Filter {
	f := s.filters[i]
	if a, ok := f["address"].(string); ok {
		return streams.Filter{Address: []string{a}}
	}
	return streams.Filter{Topics: [][]string{{f["topics"].([]interface{})[0].(string)}}}
}

// app keeps the applied chain and its log keys, and records what the
// contract forbids: a gap, a broken parent link, a key applied twice on
// one branch, an undo to a block it never applied.
type app struct {
	mu    sync.Mutex
	chain []streams.BlockRef
	held  map[string]uint64
	undos int
	bad   []string
}

func (a *app) handler() streams.Handler {
	a.held = map[string]uint64{}
	return streams.Handler{
		Apply: func(_ context.Context, b streams.Block) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if n := len(a.chain); n > 0 {
				last := a.chain[n-1]
				if b.Number != last.Number+1 || b.ParentHash != last.Hash {
					a.bad = append(a.bad, fmt.Sprintf("block %d %s after %d %s", b.Number, b.Hash, last.Number, last.Hash))
				}
			}
			for _, l := range b.Logs {
				if _, ok := a.held[l.Key()]; ok {
					a.bad = append(a.bad, "key applied twice on one branch: "+l.Key())
				}
				a.held[l.Key()] = b.Number
			}
			a.chain = append(a.chain, b.BlockRef)
			return nil
		},
		Undo: func(_ context.Context, fp streams.BlockRef) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.undos++
			i := len(a.chain) - 1
			for ; i >= 0 && a.chain[i].Number > fp.Number; i-- {
			}
			switch {
			case i >= 0 && a.chain[i] != fp:
				a.bad = append(a.bad, fmt.Sprintf("undo to %d: another branch", fp.Number))
			case i < 0 && len(a.chain) > 0 && fp.Number >= a.chain[0].Number:
				a.bad = append(a.bad, fmt.Sprintf("undo to %d: never applied", fp.Number))
			}
			a.chain = a.chain[:i+1]
			for k, n := range a.held {
				if n > fp.Number {
					delete(a.held, k)
				}
			}
			return nil
		},
	}
}

// requireServed: the applied chain is the one pod view's vendors serve,
// with exactly its keys for filter.
func (a *app) requireServed(t *testing.T, s *sim, filter, view int) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bad) > 0 {
		t.Fatalf("the app saw %d violations, first: %s", len(a.bad), a.bad[0])
	}
	if len(a.chain) < 20 {
		t.Fatalf("only %d blocks applied", len(a.chain))
	}
	from, to := a.chain[0].Number, a.chain[len(a.chain)-1].Number
	r := s.send(t, map[string]interface{}{"op": "canon", "from": from, "to": to, "filter": filter, "view": view})
	if len(r.Blocks) != len(a.chain) {
		t.Fatalf("pod %d serves %d blocks in %d-%d, the app holds %d", view, len(r.Blocks), from, to, len(a.chain))
	}
	want := map[string]bool{}
	for i, b := range r.Blocks {
		if got := a.chain[i].Hash.String(); got != b.Hash {
			t.Fatalf("block %d: applied %s, pod %d serves %s", b.Number, got, view, b.Hash)
		}
		for _, k := range b.Keys {
			want[k] = true
		}
	}
	if len(want) == 0 {
		t.Fatalf("filter %d matched no log in %d-%d", filter, from, to)
	}
	for k := range a.held {
		if !want[k] {
			t.Fatalf("key %s applied, not on pod %d's branch", k, view)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		t.Fatalf("%d keys of pod %d's branch never applied", len(want), view)
	}
}

// Two endpoints on different branches; the leader's branch reorgs below
// the point where they split, then the leader dies: the app must move to
// the other branch with an undo to the fork point and every block above
// it, none skipped. The leader's pod comes back and ends each stream
// out of range (cursor_unknown: the cursor is on a branch it never saw);
// Run must go on with the other endpoint.
func TestMergeLeaderDiesOnAnotherBranch(t *testing.T) {
	s := startSim(t)
	a := &app{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streams.Run(ctx, streams.Config{Endpoints: s.endpoints, Kind: streams.Logs, Filter: s.filter(1),
			HeartbeatSeconds: 1}, a.handler())
	}()
	s.send(t, map[string]interface{}{"op": "wait", "blocks": 10})
	s.send(t, map[string]interface{}{"op": "pin", "pod": 1, "on": true})
	s.send(t, map[string]interface{}{"op": "reorg", "depth": 3})
	s.send(t, map[string]interface{}{"op": "wait", "blocks": 6})
	s.send(t, map[string]interface{}{"op": "kill", "pod": 0})
	s.send(t, map[string]interface{}{"op": "wait", "blocks": 10})
	s.send(t, map[string]interface{}{"op": "start", "pod": 0})
	s.send(t, map[string]interface{}{"op": "wait", "blocks": 16})
	select {
	case err := <-done:
		t.Fatalf("Run ended while the other endpoint streams: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if a.undos == 0 {
		t.Fatal("no undo across the branch switch")
	}
	a.requireServed(t, s, 1, 1)
}

// A pod whose send limit is below one log's message ends each logs stream
// RESOURCE_EXHAUSTED MESSAGE_TOO_LARGE naming the block; a resume meets
// the same block, so Run must stop with *MessageTooLargeError, never
// reconnect.
func TestMessageTooLargeEndsRun(t *testing.T) {
	s := startSim(t)
	s.send(t, map[string]interface{}{"op": "kill", "pod": 0})
	s.send(t, map[string]interface{}{"op": "start", "pod": 0, "maxSendBytes": 160})
	a := &app{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := streams.Run(ctx, streams.Config{Endpoints: s.endpoints[:1], Kind: streams.Logs, Filter: s.filter(0),
		HeartbeatSeconds: 1}, a.handler())
	var tl *streams.MessageTooLargeError
	if !errors.As(err, &tl) {
		t.Fatalf("Run ended %v within 30s, want *MessageTooLargeError", err)
	}
	if tl.Block == nil || tl.MaxBytes != 160 || tl.MessageBytes <= 160 {
		t.Fatalf("Run: %v", tl)
	}
	r := s.send(t, map[string]interface{}{"op": "canon", "from": tl.Block.Number, "to": tl.Block.Number})
	if len(r.Blocks) != 1 || r.Blocks[0].Hash != tl.Block.Hash.String() {
		t.Fatalf("Run names block %s, the chain holds %v", tl.Block.Token(), r.Blocks)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.chain); n > 0 && a.chain[n-1].Number >= tl.Block.Number {
		t.Fatalf("applied block %d, at or above the block too large", a.chain[n-1].Number)
	}
}
