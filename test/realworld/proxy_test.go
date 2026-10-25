package realworld

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	gethcommon "github.com/ethereum/go-ethereum/common"
)

// linkFaults are the network faults of one (pod, vendor) link.
type linkFaults struct {
	Latency   time.Duration
	Jitter    time.Duration // extra latency per chunk, up to this; bytes keep their order
	Bandwidth int           // bytes per second each way; 0 is unlimited
	// RSTMid: once a pod request names this method, the vendor→pod chunk
	// that follows is cut half way (FIN, then RST), so the answer's body
	// ends mid-way.
	RSTMid string
	// RSTFirst, when set, is shared by every link of one pod: only the
	// first request with given params is cut, on whichever link carries
	// it, so each fetch's first attempt is cut and its failover is not.
	RSTFirst  *firstCuts
	BlackHole bool // forward nothing either way; sockets stay open (half-open)
	Refuse    bool // reset new connections at once
}

// firstCuts records the request params a cut already took.
type firstCuts struct {
	mu   sync.Mutex
	seen map[string]bool
}

// first reports whether request b is the first with its params; true for
// every request when f is nil.
func (f *firstCuts) first(b []byte) bool {
	if f == nil {
		return true
	}
	i := bytes.Index(b, []byte(`"params"`))
	if i < 0 {
		return true
	}
	key := string(b[i:])
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = make(map[string]bool)
	}
	if f.seen[key] {
		return false
	}
	f.seen[key] = true
	return true
}

// proxy stands between every pod and one vendor. Pods share its address,
// so they share the upstream's endpoint and its shared-state keys, as pods
// of one deployment do; it tells their connections apart by the process
// that owns the connecting socket, and applies each (pod, vendor) link's
// faults.
type proxy struct {
	h      *harness
	name   string
	target string
	ln     net.Listener
	ws     bool

	mu     sync.Mutex
	links  map[*pod]*link
	closed bool

	unknown atomic.Int64 // connections from no pod
}

type link struct {
	mu      sync.Mutex
	f       linkFaults
	conns   map[*pconn]bool
	accepts int64
	resets  int64 // connections RSTMid reset
}

type pconn struct {
	client, server net.Conn
	once           sync.Once
	done           chan struct{}
	// clientGone is closed once a read from the pod fails: the pod closed
	// its end.
	clientGone chan struct{}
	goneOnce   sync.Once
	// held: black-holed on its own (holdOpen), whatever the link's faults;
	// armed: a request naming the link's RSTMid method went out.
	held, armed atomic.Bool
}

// headerScan finds the block headers in one connection's vendor→pod bytes
// as they reach the pod: the first header of a block from any upstream is
// where a pod's delivery bound starts (spec §15.1, rows U3 and X9). tail
// keeps the last bytes of the previous chunk, so a header split across two
// chunks is still found. pending are the ids of the pod's
// eth_getBlockByNumber("latest") requests not answered yet; a header in the
// answer to one is a latest answer (row U18). cur is the id of the vendor
// message being read, "" in a push, which carries no id.
type headerScan struct {
	h    *harness
	pod  int
	tail []byte
	cur  string

	mu      sync.Mutex
	pending map[string]bool
}

// hashKey is how a header's own hash opens in vendor JSON: object keys are
// sorted, so "hash" never follows "blockHash" or "parentHash" here, and a
// message opens with its id, or with "jsonrpc" when it is a push.
var hashKey = []byte(`"hash":"0x`)

var (
	byNumberKey = []byte(`eth_getBlockByNumber`)
	latestKey   = []byte(`"latest"`)
	jsonrpcKey  = []byte(`"jsonrpc"`)
	idKey       = []byte(`"id":`)
	answerKey   = []byte(`{"id":`)
	pushKey     = []byte(`{"jsonrpc"`)
)

// request notes the ids of the latest block requests in a pod→vendor
// chunk; each message is the part of the chunk from one "jsonrpc" key to
// the next.
func (sc *headerScan) request(b []byte) {
	if !bytes.Contains(b, byNumberKey) || !bytes.Contains(b, latestKey) {
		return
	}
	starts := []int{}
	for i := 0; ; {
		j := bytes.Index(b[i:], jsonrpcKey)
		if j < 0 {
			break
		}
		starts = append(starts, i+j)
		i += j + len(jsonrpcKey)
	}
	if len(starts) == 0 {
		return
	}
	starts[0] = 0
	for k, from := range starts {
		to := len(b)
		if k+1 < len(starts) {
			to = starts[k+1]
		}
		msg := b[from:to]
		if !bytes.Contains(msg, byNumberKey) || !bytes.Contains(msg, latestKey) {
			continue
		}
		if id, ok := jsonID(msg, bytes.Index(msg, idKey)); ok {
			sc.mu.Lock()
			if sc.pending == nil {
				sc.pending = make(map[string]bool)
			}
			sc.pending[id] = true
			sc.mu.Unlock()
		}
	}
}

// jsonID is the raw id value whose "id": key starts at at in b; false when
// there is none or it is cut off.
func jsonID(b []byte, at int) (string, bool) {
	if at < 0 {
		return "", false
	}
	v := b[at+len(idKey):]
	end := bytes.IndexAny(v, ",}")
	if end < 0 {
		return "", false
	}
	return string(bytes.TrimSpace(v[:end])), true
}

// answers reports whether id names a pending latest request, which it
// then no longer is.
func (sc *headerScan) answers(id string) bool {
	if id == "" {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.pending[id] {
		return false
	}
	delete(sc.pending, id)
	return true
}

// scan records every header hash in b, written to the pod at at.
func (sc *headerScan) scan(b []byte, at time.Time) {
	buf := append(sc.tail, b...)
	keepFrom := -1
scan:
	for i := 0; ; {
		h, a, p := index(buf, i, hashKey), index(buf, i, answerKey), index(buf, i, pushKey)
		next := -1
		for _, x := range []int{h, a, p} {
			if x >= 0 && (next < 0 || x < next) {
				next = x
			}
		}
		switch {
		case next < 0:
			break scan
		case next == a:
			id, ok := jsonID(buf, a+1)
			if !ok {
				keepFrom = a
				break scan
			}
			sc.cur = id
			i = a + len(answerKey)
		case next == p:
			sc.cur = ""
			i = p + len(pushKey)
		default:
			start := h + len(hashKey)
			if start+64 > len(buf) {
				break scan
			}
			if hx := buf[start : start+64]; isHex(hx) {
				sc.h.sawHeader(sc.pod, gethcommon.HexToHash(string(hx)), at, sc.answers(sc.cur), sc.cur == "")
			}
			i = start
		}
	}
	keep := min(len(buf), len(hashKey)+64)
	if keepFrom >= 0 {
		keep = max(keep, len(buf)-keepFrom)
	}
	sc.tail = append(sc.tail[:0], buf[len(buf)-keep:]...)
}

// index is the position of key in b at or after from, -1 when absent.
func index(b []byte, from int, key []byte) int {
	if j := bytes.Index(b[from:], key); j >= 0 {
		return from + j
	}
	return -1
}

func isHex(b []byte) bool {
	for _, c := range b {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func (c *pconn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.client.Close()
		_ = c.server.Close()
	})
}

// reset cuts the connection: a FIN to the pod right after the half answer,
// so it reads a truncated body and EOF at once, then RST on both sides once
// the pod closed its end, or after cutGrace. The FIN carries the cut: a
// host may drop the RST (macOS with net.inet.tcp.blackhole set leaves the
// pod's socket ESTABLISHED), and a pod would then wait out its attempt.
func (c *pconn) reset() {
	if tc, ok := c.client.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	select {
	case <-c.clientGone:
	case <-time.After(cutGrace):
	}
	for _, s := range []net.Conn{c.client, c.server} {
		if tc, ok := s.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	}
	c.close()
}

// cutGrace bounds how long a cut waits for the pod to close its end
// before the RST.
const cutGrace = time.Second

func (h *harness) newProxy(name, target string, ws bool) *proxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	px := &proxy{h: h, name: name, target: target, ln: ln, ws: ws, links: make(map[*pod]*link)}
	h.mu.Lock()
	h.proxies = append(h.proxies, px)
	h.mu.Unlock()
	go px.serve()
	return px
}

// URL is the upstream endpoint every pod's config names.
func (px *proxy) URL() string {
	if px.ws {
		return "ws://" + px.ln.Addr().String()
	}
	return "http://" + px.ln.Addr().String()
}

func (px *proxy) link(p *pod) *link {
	px.mu.Lock()
	defer px.mu.Unlock()
	l := px.links[p]
	if l == nil {
		l = &link{conns: make(map[*pconn]bool)}
		px.links[p] = l
	}
	return l
}

// set replaces the faults of p's link.
func (px *proxy) set(p *pod, f linkFaults) {
	l := px.link(p)
	l.mu.Lock()
	l.f = f
	l.mu.Unlock()
}

// accepts counts the connections p opened through this proxy.
func (px *proxy) accepts(p *pod) int64 {
	l := px.link(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// resets counts p's connections that RSTMid reset.
func (px *proxy) resets(p *pod) int64 {
	l := px.link(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resets
}

// holdOpen black-holes every connection p's link has open now, and keeps
// their sockets, as a keep-alive pool whose path died; connections opened
// later pass. It returns how many it held.
func (px *proxy) holdOpen(p *pod) int {
	l := px.link(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		c.held.Store(true)
	}
	return len(l.conns)
}

// closeAll closes every open connection of p's link.
func (px *proxy) closeAll(p *pod) {
	l := px.link(p)
	l.mu.Lock()
	var cs []*pconn
	for c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	for _, c := range cs {
		c.close()
	}
}

func (px *proxy) close() {
	px.mu.Lock()
	px.closed = true
	links := make([]*link, 0, len(px.links))
	for _, l := range px.links {
		links = append(links, l)
	}
	px.mu.Unlock()
	_ = px.ln.Close()
	for _, l := range links {
		l.mu.Lock()
		var cs []*pconn
		for c := range l.conns {
			cs = append(cs, c)
		}
		l.mu.Unlock()
		for _, c := range cs {
			c.close()
		}
	}
}

func (px *proxy) serve() {
	for {
		c, err := px.ln.Accept()
		if err != nil {
			return
		}
		go px.handle(c)
	}
}

func (px *proxy) handle(c net.Conn) {
	start := time.Now()
	p := px.podOf(c)
	px.h.lookups.took(time.Since(start))
	if p == nil {
		px.unknown.Add(1)
		_ = c.Close()
		return
	}
	l := px.link(p)
	l.mu.Lock()
	l.accepts++
	f := l.f
	l.mu.Unlock()
	if f.Refuse {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
		return
	}
	s, err := net.DialTimeout("tcp", px.target, 5*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	pc := &pconn{client: c, server: s, done: make(chan struct{}), clientGone: make(chan struct{})}
	l.mu.Lock()
	l.conns[pc] = true
	l.mu.Unlock()
	sc := &headerScan{h: px.h, pod: p.idx}
	go l.pipe(pc, s, c, sc, nil)
	l.pipe(pc, c, s, nil, sc)
	<-pc.done
	l.mu.Lock()
	delete(l.conns, pc)
	l.mu.Unlock()
}

// podOf is the pod whose process owns the socket at c's remote end.
func (px *proxy) podOf(c net.Conn) *pod {
	from, to := c.RemoteAddr().(*net.TCPAddr).Port, px.ln.Addr().(*net.TCPAddr).Port
	for range 5 {
		if p := px.h.podOfPid(socketOwner(from, to, px.h.podPids())); p != nil {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// lookups are how long each proxied connection waited to be told apart.
type lookups struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (st *lookups) took(d time.Duration) {
	st.mu.Lock()
	st.waits = append(st.waits, d)
	st.mu.Unlock()
}

// summary is the count, median and maximum of the waits.
func (st *lookups) summary() string {
	st.mu.Lock()
	w := slices.Clone(st.waits)
	st.mu.Unlock()
	if len(w) == 0 {
		return "no connections"
	}
	slices.Sort(w)
	return fmt.Sprintf("%d connections, wait p50 %s max %s", len(w), w[len(w)/2].Round(time.Millisecond), w[len(w)-1].Round(time.Millisecond))
}

func (l *link) faults() linkFaults {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f
}

// waitOpen waits while the link or the connection is black-holed; false
// once pc closed.
func (l *link) waitOpen(pc *pconn) bool {
	for l.faults().BlackHole || pc.held.Load() {
		select {
		case <-pc.done:
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-pc.done:
		return false
	default:
		return true
	}
}

type chunk struct {
	b   []byte
	due time.Time
}

// pipe forwards src to dst under the link's faults; down (non-nil) is the
// vendor→pod direction, whose headers it records as they reach the pod, up
// (non-nil) the pod→vendor one, whose latest requests it notes. Reads stop
// while black-holed, so TCP backpressure builds as on a dead path.
func (l *link) pipe(pc *pconn, src, dst net.Conn, down, up *headerScan) {
	ch := make(chan chunk, 256)
	go func() {
		defer pc.close()
		for ck := range ch {
			if d := time.Until(ck.due); d > 0 {
				select {
				case <-pc.done:
					return
				case <-time.After(d):
				}
			}
			if !l.waitOpen(pc) {
				return
			}
			f := l.faults()
			b := ck.b
			if down != nil && f.RSTMid != "" && pc.armed.Load() {
				_, _ = dst.Write(b[:len(b)/2])
				l.mu.Lock()
				l.resets++
				l.mu.Unlock()
				pc.reset()
				return
			}
			if up != nil && f.RSTMid != "" && bytes.Contains(b, []byte(f.RSTMid)) && f.RSTFirst.first(b) {
				pc.armed.Store(true)
			}
			if _, err := dst.Write(b); err != nil {
				return
			}
			if down != nil {
				down.scan(b, time.Now())
			}
			if up != nil {
				up.request(b)
			}
			if f.Bandwidth > 0 {
				select {
				case <-pc.done:
					return
				case <-time.After(time.Duration(len(b)) * time.Second / time.Duration(f.Bandwidth)):
				}
			}
		}
	}()
	defer close(ch)
	buf := make([]byte, 32<<10)
	var last time.Time
	for {
		if !l.waitOpen(pc) {
			return
		}
		n, err := src.Read(buf)
		if n > 0 {
			f := l.faults()
			due := time.Now().Add(f.Latency)
			if f.Jitter > 0 {
				due = due.Add(rand.N(f.Jitter))
			}
			if due.Before(last) {
				due = last
			}
			last = due
			select {
			case ch <- chunk{append([]byte(nil), buf[:n]...), due}:
			case <-pc.done:
				return
			}
		}
		if err != nil {
			if up != nil {
				pc.goneOnce.Do(func() { close(pc.clientGone) })
			}
			return
		}
	}
}
