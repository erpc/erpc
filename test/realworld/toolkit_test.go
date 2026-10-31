package realworld

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/erpc/erpc/sdk/go/streams"
	"github.com/erpc/erpc/test/chainsim"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc/status"
)

// Toolkit rows (specs/streams/feature.md §9, §13 T1, T3–T6). The Go
// library runs in this process (startGoClient); the TS library runs as a
// subprocess (toolkit_node_test.go). Each driver appends one JSON record
// per line to its client's file, and judgeToolkit holds them as an app
// would.

// toolkitBeat is the toolkit clients' heartbeatSeconds; the watchdog
// reconnects after 3 of them.
const toolkitBeat = 2

// toolkitRecord is one record a toolkit driver writes: t is apply, undo,
// status or end; at is unix milliseconds. apply: the block and its keys
// (logs Log.Key, heads the hash); undo: the fork point; status: state,
// reason, endpoint; end: Run returned, error null on stop, else its code
// (OUT_OF_RANGE with cursor and oldest).
type toolkitRecord struct {
	T          string      `json:"t"`
	At         int64       `json:"at"`
	Number     uint64      `json:"number,omitempty"`
	Hash       string      `json:"hash,omitempty"`
	ParentHash string      `json:"parentHash,omitempty"`
	Keys       []string    `json:"keys,omitempty"`
	State      string      `json:"state,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Endpoint   string      `json:"endpoint,omitempty"`
	Error      *string     `json:"error"`
	Code       string      `json:"code,omitempty"`
	Cursor     string      `json:"cursor,omitempty"`
	Oldest     *toolkitRef `json:"oldest,omitempty"`
}

type toolkitRef struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

func (r toolkitRecord) at() time.Time { return time.UnixMilli(r.At) }

// toolkitClient is one toolkit app: its records file.
type toolkitClient struct {
	s    *scenario
	name string
	path string

	mu sync.Mutex
}

func (s *scenario) newToolkitClient(name string) *toolkitClient {
	return &toolkitClient{s: s, name: name, path: filepath.Join(s.dir, strings.ReplaceAll(name, "/", "-")+".jsonl")}
}

func (c *toolkitClient) write(r toolkitRecord) {
	r.At = time.Now().UnixMilli()
	b, err := json.Marshal(r)
	if err != nil {
		c.s.violate(c.name, "record: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		c.s.violate(c.name, "record: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		c.s.violate(c.name, "record: %v", err)
	}
}

// records are the client's records so far; a line that does not parse is
// a violation.
func (c *toolkitClient) records() []toolkitRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.s.violate(c.name, "records: %v", err)
		return nil
	}
	defer f.Close()
	var out []toolkitRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var r toolkitRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			c.s.violate(c.name, "record %q: %v", sc.Text(), err)
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *scenario) grpcEndpoint(pod int) string {
	return fmt.Sprintf("grpc://%s/%s/evm/%d", s.h.pods[pod].grpcAddr(), s.project, s.chain.ID())
}

func toolkitFilter(i int) streams.Filter {
	f := chainsim.Filters[i]
	if f.Address != nil {
		return streams.Filter{Address: []string{f.Address.Hex()}}
	}
	return streams.Filter{Topics: [][]string{{f.Topic0.Hex()}}}
}

var errAppKilled = errors.New("app killed after apply, before the cursor save")

// toolkitStarter starts a toolkit library for c with cfg, its cursor in
// the file at cursorPath ("" none), until stop or a terminal end.
// killEvery > 0 kills the app at every killEvery-th apply, after the apply
// and before the cursor save, and starts it again.
type toolkitStarter func(s *scenario, c *toolkitClient, cfg streams.Config, cursorPath string, killEvery int) (stop func())

// startGoClient is the Go library's toolkitStarter.
func (s *scenario) startGoClient(c *toolkitClient, cfg streams.Config, cursorPath string, killEvery int) (stop func()) {
	if cursorPath != "" {
		cfg.Cursor = streams.FileCursor(cursorPath)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		applies := 0
		for {
			killed := false
			err := streams.Run(ctx, cfg, streams.Handler{
				Apply: func(_ context.Context, b streams.Block) error {
					r := toolkitRecord{T: "apply", Number: b.Number, Hash: b.Hash.String(), ParentHash: b.ParentHash.String()}
					switch {
					case cfg.Kind == streams.Logs:
						r.Keys = []string{}
						for _, l := range b.Logs {
							r.Keys = append(r.Keys, l.Key())
						}
					case cfg.Kind == streams.NewHeads:
						r.Keys = []string{b.Hash.String()}
					}
					c.write(r)
					if applies++; killEvery > 0 && applies%killEvery == 0 {
						killed = true
						return errAppKilled
					}
					return nil
				},
				Undo: func(_ context.Context, fp streams.BlockRef) error {
					c.write(toolkitRecord{T: "undo", Number: fp.Number, Hash: fp.Hash.String()})
					return nil
				},
				Status: func(st streams.Status) {
					c.write(toolkitRecord{T: "status", State: string(st.State), Reason: st.Reason, Endpoint: st.Endpoint})
				},
			})
			c.write(endRecord(err))
			if !killed || ctx.Err() != nil {
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}

func endRecord(err error) toolkitRecord {
	r := toolkitRecord{T: "end"}
	if err == nil {
		return r
	}
	msg := err.Error()
	r.Error = &msg
	var oor *streams.OutOfRangeError
	var fr *streams.FinalRevertedError
	var tl *streams.MessageTooLargeError
	switch {
	case errors.As(err, &oor):
		r.Code, r.Cursor = "OUT_OF_RANGE", oor.Cursor
		if oor.Oldest != nil {
			r.Oldest = &toolkitRef{Number: oor.Oldest.Number, Hash: oor.Oldest.Hash.String()}
		}
	case errors.As(err, &fr):
		r.Code = "ABORTED"
	case errors.As(err, &tl):
		r.Code = "RESOURCE_EXHAUSTED"
	default:
		if st, ok := status.FromError(err); ok {
			var b strings.Builder
			prev := 'A'
			for _, ch := range st.Code().String() {
				if unicode.IsUpper(ch) && unicode.IsLower(prev) {
					b.WriteByte('_')
				}
				b.WriteRune(unicode.ToUpper(ch))
				prev = ch
			}
			r.Code = b.String()
		}
	}
	return r
}

// toolkitView is what judgeToolkit read.
type toolkitView struct {
	applies, undos, statuses, ends []toolkitRecord
	// repeats: applies of the last block again right after an end, as an
	// app killed before its cursor save gets.
	repeats     int
	first, last uint64
}

// judgeToolkit holds c's records in order as an app would: each apply
// links to the last block or undo, or right after an end repeats the last
// block with the same keys; no other block twice (simulated reorgs never
// bring a branch back); each undo below the last block. The keys held over
// the applied blocks are the canonical chain's: filter's logs, or for -1
// the heads.
func (s *scenario) judgeToolkit(c *toolkitClient, filter int) toolkitView {
	var v toolkitView
	var top *toolkitRecord
	keysOf := map[string][]string{}
	held := map[string]uint64{}
	heads := map[uint64]string{}
	restarted := false
	for _, r := range c.records() {
		switch r.T {
		case "apply":
			v.applies = append(v.applies, r)
			if top != nil && restarted && r.Hash == top.Hash {
				if strings.Join(keysOf[r.Hash], ",") != strings.Join(r.Keys, ",") {
					s.violate(c.name, "block %d applied again with other keys", r.Number)
				}
				v.repeats++
				restarted = false
				continue
			}
			restarted = false
			if top != nil && (r.Number != top.Number+1 || r.ParentHash != top.Hash) {
				s.violate(c.name, "block %d %s parent %s applied on %d %s", r.Number, shortHex(r.Hash), shortHex(r.ParentHash), top.Number, shortHex(top.Hash))
			}
			if _, ok := keysOf[r.Hash]; ok {
				s.violate(c.name, "block %d %s applied twice", r.Number, shortHex(r.Hash))
			}
			keysOf[r.Hash] = r.Keys
			if v.first == 0 {
				v.first = r.Number
			}
			heads[r.Number] = r.Hash
			for _, k := range r.Keys {
				held[k] = r.Number
			}
			rr := r
			top = &rr
		case "undo":
			v.undos = append(v.undos, r)
			if top != nil && r.Number >= top.Number {
				s.violate(c.name, "undo to %d at %d", r.Number, top.Number)
			}
			for n := range heads {
				if n > r.Number {
					delete(heads, n)
				}
			}
			for k, n := range held {
				if n > r.Number {
					delete(held, k)
				}
			}
			rr := r
			top = &rr
			restarted = false
		case "status":
			v.statuses = append(v.statuses, r)
		case "end":
			v.ends = append(v.ends, r)
			restarted = true
		default:
			s.violate(c.name, "record type %q", r.T)
		}
	}
	if top == nil || v.first == 0 {
		s.violate(c.name, "applied nothing")
		return v
	}
	v.last = top.Number
	for n := v.first; n <= v.last; n++ {
		if want := s.chain.CanonAt(n).Hash.Hex(); heads[n] != want {
			s.violate(c.name, "block %d: holds %s, canonical %s", n, shortHex(heads[n]), shortHex(want))
			break
		}
	}
	if filter < 0 {
		return v
	}
	want := map[string]bool{}
	for k := range s.chain.CanonicalLogs(chainsim.Filters[filter], v.first, v.last) {
		want[fmt.Sprintf("%s:%d", k.BlockHash.Hex(), k.LogIndex)] = true
	}
	for k, n := range held {
		if !want[k] {
			s.violate(c.name, "holds log %s of block %d, not canonical", k, n)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		s.violate(c.name, "misses %d canonical logs of blocks %d-%d", len(want), v.first, v.last)
	}
	return v
}

// awaitToolkitTip waits until c's last record is an apply of the canonical
// tip, as a client that caught up.
func (s *scenario) awaitToolkitTip(c *toolkitClient) {
	deadline := time.Now().Add(settleDelivery + reconnectSlack)
	for {
		recs := c.records()
		tip := s.chain.Tip().Hash.Hex()
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].T == "apply" {
				if recs[i].Hash == tip {
					return
				}
				break
			}
		}
		if time.Now().After(deadline) {
			s.violate(c.name, "not at the canonical tip %s within %s", shortHex(tip), settleDelivery+reconnectSlack)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// firstApplyAfter is the first apply after t, if any.
func (v toolkitView) firstApplyAfter(t time.Time) (toolkitRecord, bool) {
	for _, r := range v.applies {
		if !r.at().Before(t) {
			return r, true
		}
	}
	return toolkitRecord{}, false
}

// T1 (Go), T2 (TS): the library on both pods, merged, under C1 reorgs;
// pod-1 is killed with SIGKILL and restarts. The app's logs are canonical,
// each block applied once per branch, and it applies from pod-2 within
// the watchdog of the kill.
func t1(start toolkitStarter, name string) spec {
	return spec{name: name, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		c := s.newToolkitClient(s.name + "/app")
		stop := start(s, c, streams.Config{Endpoints: []string{s.grpcEndpoint(0), s.grpcEndpoint(1)}, Kind: streams.Logs,
			Filter: toolkitFilter(0), HeartbeatSeconds: toolkitBeat}, filepath.Join(s.dir, "app.cursor"), 0)
		for range 4 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(10)
		ex := s.excuse(0, time.Now())
		killed := time.Now()
		s.h.pods[0].kill()
		s.logf("pod-1 killed")
		for range 3 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		if err := s.h.pods[0].start(); err != nil {
			s.t.Fatalf("pod-1 restart: %v", err)
		}
		s.waitServing(0)
		s.endExcuse(ex, time.Now().Add(liveBound))
		for range 4 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(20)
		s.awaitToolkitTip(c)
		stop()
		v := s.judgeToolkit(c, 0)
		switchBound := 3*toolkitBeat*time.Second + reconnectSlack
		if r, ok := v.firstApplyAfter(killed); !ok || r.at().Sub(killed) > switchBound {
			s.violate(c.name, "no apply within %s of pod-1's kill", switchBound)
		}
		if len(v.undos) == 0 {
			s.violate(c.name, "no undo across 11 reorgs")
		}
	}}
}

// T3: pod-1's link black-holed with its sockets kept: the watchdog
// reconnects within 3 heartbeats; once the link returns the app catches
// up with no gap.
func t3(start toolkitStarter, name string) spec {
	return spec{name: name, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		link := s.newGrpcLink()
		c := s.newToolkitClient(s.name + "/app")
		stop := start(s, c, streams.Config{Endpoints: []string{link.grpcEndpoint(s, 0)}, Kind: streams.Logs,
			Filter: toolkitFilter(1), HeartbeatSeconds: toolkitBeat}, "", 0)
		s.blocks(30)
		s.reorg(2)
		s.blocks(15)
		held := time.Now()
		link.hold(true)
		watchdog := 3 * toolkitBeat * time.Second
		time.Sleep(watchdog + reconnectSlack)
		link.hold(false)
		s.blocks(30)
		s.reorg(3)
		s.blocks(25)
		s.awaitToolkitTip(c)
		stop()
		v := s.judgeToolkit(c, 1)
		seen := false
		for _, r := range v.statuses {
			if r.State == string(streams.StateReconnecting) && r.Reason == "watchdog" && !r.at().Before(held) {
				seen = true
				if d := r.at().Sub(held); d > watchdog+time.Second {
					s.violate(c.name, "watchdog reconnect %s after the black-hole, bound %s", d, watchdog)
				}
				break
			}
		}
		if !seen {
			s.violate(c.name, "no watchdog reconnect after the black-hole")
		}
	}}
}

// T4 (Go): the app is killed after every 7th apply, before its cursor
// save, and starts again: that block is applied again with the same keys;
// the final logs are canonical.
func t4() spec {
	return spec{name: "T4", bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		c := s.newToolkitClient(s.name + "/app")
		stop := s.startGoClient(c, streams.Config{Endpoints: []string{s.grpcEndpoint(0)}, Kind: streams.Logs,
			Filter: toolkitFilter(0), HeartbeatSeconds: toolkitBeat}, filepath.Join(s.dir, "app.cursor"), 7)
		for range 6 {
			s.blocks(15)
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(20)
		s.awaitToolkitTip(c)
		stop()
		v := s.judgeToolkit(c, 0)
		if v.repeats < 5 {
			s.violate(c.name, "%d blocks applied again after a kill, want one per kill (≥ 5)", v.repeats)
		}
	}}
}

// T5: the app stays offline longer than the window: on its return Run ends
// with OutOfRangeError naming its cursor and the oldest block a pod can
// resume from, and applies nothing past the gap.
func t5(start toolkitStarter, name string) spec {
	return spec{name: name, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		c := s.newToolkitClient(s.name + "/app")
		cfg := streams.Config{Endpoints: []string{s.grpcEndpoint(0)}, Kind: streams.Logs, Filter: toolkitFilter(0), HeartbeatSeconds: toolkitBeat}
		cursorPath := filepath.Join(s.dir, "app.cursor")
		stop := start(s, c, cfg, cursorPath, 0)
		s.blocks(30)
		stop()
		offline := time.Now()
		recs := c.records()
		var cursor string
		for _, r := range recs {
			if r.T == "apply" || r.T == "undo" {
				cursor = fmt.Sprintf("0x%x:%s", r.Number, r.Hash)
			}
		}
		time.Sleep(maxBackfill + 30*time.Second)
		s.logf("app offline %s, back", time.Since(offline).Round(time.Second))
		back := time.Now()
		stop = start(s, c, cfg, cursorPath, 0)
		deadline := time.Now().Add(30 * time.Second)
		var end *toolkitRecord
		for end == nil && time.Now().Before(deadline) {
			for _, r := range c.records() {
				if r.T == "end" && !r.at().Before(back) {
					rr := r
					end = &rr
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		stop()
		switch {
		case end == nil:
			s.violate(c.name, "Run did not end within 30s of a resume older than the window")
		case end.Code != "OUT_OF_RANGE":
			s.violate(c.name, "Run ended %s (%v), want OUT_OF_RANGE", end.Code, end.Error)
		case end.Cursor != cursor:
			s.violate(c.name, "OutOfRangeError cursor %s, the app's last block is %s", end.Cursor, cursor)
		case end.Oldest == nil:
			s.violate(c.name, "OutOfRangeError names no oldest block")
		}
		v := s.judgeToolkit(c, 0)
		if r, ok := v.firstApplyAfter(back); ok {
			s.violate(c.name, "applied block %d past the gap", r.Number)
		}
	}}
}

// t6Age and t6Run: T6's connection age and run, the spec's 10 and 25
// minutes scaled to keep its ratio, so the run fits a 10-minute command.
const (
	t6Age = 90 * time.Second
	t6Run = 5 * time.Minute
)

// T6: a finalized stream for t6Run under a t6Age connection age: at least
// 2 UNAVAILABLE ends, each resumed at once; no gap, no duplicate key. A
// resume shows as an apply only once Finalized moves, so it moves each
// finalizedPoll.
func t6(start toolkitStarter, name string) spec {
	vars := map[string]time.Duration{"erpc.GrpcMaxConnectionAge": t6Age}
	return spec{name: name, bound: healthyDelivery, vars: vars, upEvm: finalizedEvery(finalizedPoll), clients: func(*scenario) {}, body: func(s *scenario) {
		s.blocks(25)
		c := s.newToolkitClient(s.name + "/app")
		stop := start(s, c, streams.Config{Endpoints: []string{s.grpcEndpoint(0)}, Kind: streams.Logs, Filter: toolkitFilter(0),
			Finality: streams.Finalized, HeartbeatSeconds: toolkitBeat}, filepath.Join(s.dir, "app.cursor"), 0)
		for end := time.Now().Add(t6Run); time.Now().Before(end); {
			s.blocks(50)
		}
		stop()
		v := s.judgeToolkit(c, 0)
		ends := 0
		for _, r := range v.statuses {
			if r.State != string(streams.StateReconnecting) || r.Reason != "UNAVAILABLE" {
				continue
			}
			ends++
			if next, ok := v.firstApplyAfter(r.at()); !ok || next.at().Sub(r.at()) > 5*blockTime+time.Second {
				s.violate(c.name, "no apply within %s of the UNAVAILABLE end at %s", 5*blockTime+time.Second, r.at().Format("15:04:05.000"))
			}
		}
		if ends < 2 {
			s.violate(c.name, "%d UNAVAILABLE ends in %s, want ≥ 2 from the %s connection age", ends, t6Run, t6Age)
		}
	}}
}

func shortHex(h string) string { return short(gethcommon.HexToHash(h)) }
