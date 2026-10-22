package realworld

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	p := px.podOf(c)
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

// podOf is the pod whose process owns the socket at c's remote end, as
// netstat -v reports each socket's process.
func (px *proxy) podOf(c net.Conn) *pod {
	local := fmt.Sprintf("127.0.0.1.%d", c.RemoteAddr().(*net.TCPAddr).Port)
	foreign := fmt.Sprintf("127.0.0.1.%d", px.ln.Addr().(*net.TCPAddr).Port)
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, _ := exec.CommandContext(ctx, "netstat", "-anv", "-p", "tcp").Output()
		cancel()
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 || f[3] != local || f[4] != foreign {
				continue
			}
			for _, m := range pidField.FindAllStringSubmatch(line, -1) {
				pid, _ := strconv.Atoi(m[1])
				if p := px.h.podOfPid(pid); p != nil {
					return p
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// pidField is netstat's process:pid column; process names may hold spaces.
var pidField = regexp.MustCompile(`:(\d+)\s`)

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
