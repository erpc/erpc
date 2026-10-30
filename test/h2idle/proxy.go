// Package h2idle is a TCP proxy for HTTP/2 that closes a connection once
// no frame but PING crossed it for an idle timeout, as an AWS ALB does
// (streams spec §3.6): HTTP/2 PING keeps no connection open through it.
package h2idle

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const framePing = 0x6

var clientPreface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// Proxy forwards connections to target.
type Proxy struct {
	ln     net.Listener
	target string
	idle   time.Duration
	cuts   atomic.Int64
}

// Start listens on a loopback port and forwards each connection to target.
func Start(target string, idle time.Duration) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln, target: target, idle: idle}
	go p.serve()
	return p, nil
}

// Addr is the proxy's address.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Close stops accepting; open connections run until they idle out.
func (p *Proxy) Close() { _ = p.ln.Close() }

// Cuts is how many connections the proxy closed for idleness.
func (p *Proxy) Cuts() int64 { return p.cuts.Load() }

func (p *Proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.pipe(c)
	}
}

func (p *Proxy) pipe(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			_ = client.Close()
			_ = server.Close()
		})
	}
	go func() {
		defer stop()
		if _, err := io.ReadFull(client, make([]byte, len(clientPreface))); err != nil {
			return
		}
		if _, err := server.Write(clientPreface); err != nil {
			return
		}
		frames(client, server, &last)
	}()
	go func() {
		defer stop()
		frames(server, client, &last)
	}()
	tick := time.NewTicker(p.idle / 10)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) >= p.idle {
				p.cuts.Add(1)
				stop()
				return
			}
		}
	}
}

// frames copies whole frames from src to dst; every frame but PING marks
// the connection active.
func frames(src, dst net.Conn, last *atomic.Int64) {
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			return
		}
		n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		payload := make([]byte, n)
		if _, err := io.ReadFull(src, payload); err != nil {
			return
		}
		if hdr[3] != framePing {
			last.Store(time.Now().UnixNano())
		}
		if _, err := dst.Write(append(hdr, payload...)); err != nil {
			return
		}
	}
}
