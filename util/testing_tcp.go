package util

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// SlowTCPProxy forwards TCP to a target and holds every client->server chunk
// for the set delay, so a real client sees a slow server.
type SlowTCPProxy struct {
	ln    net.Listener
	delay atomic.Int64
}

func NewSlowTCPProxy(t testing.TB, target string) *SlowTCPProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &SlowTCPProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, target)
		}
	}()
	return p
}

func (p *SlowTCPProxy) Addr() string { return p.ln.Addr().String() }

func (p *SlowTCPProxy) SetDelay(d time.Duration) { p.delay.Store(int64(d)) }

func (p *SlowTCPProxy) serve(c net.Conn, target string) {
	defer c.Close()
	s, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer s.Close()
	// Server gone: drop the client too, as a dead server's reset would.
	go func() { _, _ = io.Copy(c, s); _ = c.Close() }()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if d := time.Duration(p.delay.Load()); d > 0 {
				time.Sleep(d)
			}
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
