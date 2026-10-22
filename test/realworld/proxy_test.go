package realworld

import (
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// linkFaults are the network faults of one (pod, vendor) link.
type linkFaults struct {
	Latency   time.Duration
	Jitter    time.Duration // extra latency per chunk, up to this; bytes keep their order
	Bandwidth int           // bytes per second each way; 0 is unlimited
	RSTAfter  int64         // reset a connection once it carried this many vendor→pod bytes
	BlackHole bool          // forward nothing either way; sockets stay open (half-open)
	Refuse    bool          // reset new connections at once
}

// proxy stands between the pods and one vendor with one listener per pod:
// each pod's config names its own link, so a fault on (pod, vendor) touches
// only that pod's sockets. The endpoints differ per pod, so the pods keep
// separate per-upstream shared-state counters (common.UniqueUpstreamKey
// hashes the endpoint); network counters stay shared.
type proxy struct {
	h      *harness
	target string
	ws     bool

	mu    sync.Mutex
	links map[*pod]*link
}

type link struct {
	ln     net.Listener
	target string

	mu      sync.Mutex
	f       linkFaults
	conns   map[*pconn]bool
	accepts int64
}

type pconn struct {
	client, server net.Conn
	once           sync.Once
	done           chan struct{}
}

func (c *pconn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.client.Close()
		_ = c.server.Close()
	})
}

// reset closes both sides with RST.
func (c *pconn) reset() {
	for _, s := range []net.Conn{c.client, c.server} {
		if tc, ok := s.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	}
	c.close()
}

func (h *harness) newProxy(target string, ws bool) *proxy {
	px := &proxy{h: h, target: target, ws: ws, links: make(map[*pod]*link)}
	h.mu.Lock()
	h.proxies = append(h.proxies, px)
	h.mu.Unlock()
	return px
}

// URL is the endpoint pod p's config names for the vendor: its own link.
func (px *proxy) URL(p *pod) string {
	addr := px.link(p).ln.Addr().String()
	if px.ws {
		return "ws://" + addr
	}
	return "http://" + addr
}

// link is p's link, listening from the first time p's config names it.
func (px *proxy) link(p *pod) *link {
	px.mu.Lock()
	defer px.mu.Unlock()
	l := px.links[p]
	if l == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			px.h.t.Fatal(err)
		}
		l = &link{ln: ln, target: px.target, conns: make(map[*pconn]bool)}
		px.links[p] = l
		go l.serve()
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

// accepts counts the connections p opened through its link.
func (px *proxy) accepts(p *pod) int64 {
	l := px.link(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// closeAll closes every open connection of p's link.
func (px *proxy) closeAll(p *pod) { px.link(p).closeAll() }

func (px *proxy) close() {
	px.mu.Lock()
	links := make([]*link, 0, len(px.links))
	for _, l := range px.links {
		links = append(links, l)
	}
	px.mu.Unlock()
	for _, l := range links {
		_ = l.ln.Close()
		l.closeAll()
	}
}

func (l *link) closeAll() {
	l.mu.Lock()
	cs := make([]*pconn, 0, len(l.conns))
	for c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	for _, c := range cs {
		c.close()
	}
}

func (l *link) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.handle(c)
	}
}

func (l *link) handle(c net.Conn) {
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
	s, err := net.DialTimeout("tcp", l.target, 5*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	pc := &pconn{client: c, server: s, done: make(chan struct{})}
	l.mu.Lock()
	l.conns[pc] = true
	l.mu.Unlock()
	go l.pipe(pc, s, c, true)
	l.pipe(pc, c, s, false)
	<-pc.done
	l.mu.Lock()
	delete(l.conns, pc)
	l.mu.Unlock()
}

func (l *link) faults() linkFaults {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f
}

// waitOpen waits while the link is black-holed; false once pc closed.
func (l *link) waitOpen(pc *pconn) bool {
	for l.faults().BlackHole {
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

// pipe forwards src to dst under the link's faults; down is the vendor→pod
// direction. Reads stop while black-holed, so TCP backpressure builds as on
// a dead path.
func (l *link) pipe(pc *pconn, src, dst net.Conn, down bool) {
	ch := make(chan chunk, 256)
	go func() {
		defer pc.close()
		var sent int64
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
			if down && f.RSTAfter > 0 && sent+int64(len(b)) >= f.RSTAfter {
				_, _ = dst.Write(b[:max(0, f.RSTAfter-sent)])
				pc.reset()
				return
			}
			if _, err := dst.Write(b); err != nil {
				return
			}
			sent += int64(len(b))
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
			return
		}
	}
}
