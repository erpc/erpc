// Package h2idle is a TCP proxy for HTTP/2 that closes a connection once
// no frame but PING crossed it for an idle timeout, as an AWS ALB does
// (streams spec §3.6): HTTP/2 PING keeps no connection open through it.
// It records the flow-control windows the server announces.
package h2idle

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameSettings     = 0x4
	framePing         = 0x6
	frameWindowUpdate = 0x8
	settingsAck       = 0x1
	settingInitWindow = 0x4
)

var clientPreface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// Proxy forwards connections to target.
type Proxy struct {
	ln     net.Listener
	target string
	idle   time.Duration
	cuts   atomic.Int64

	mu            sync.Mutex
	streamWindow  uint32 // SETTINGS_INITIAL_WINDOW_SIZE the server sent
	connIncrement uint32 // the server's first connection WINDOW_UPDATE
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

// ServerWindows are the server's SETTINGS_INITIAL_WINDOW_SIZE and its
// first connection WINDOW_UPDATE increment, 0 until seen.
func (p *Proxy) ServerWindows() (stream, connIncrement uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamWindow, p.connIncrement
}

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
		p.frames(client, server, &last, false)
	}()
	go func() {
		defer stop()
		p.frames(server, client, &last, true)
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
func (p *Proxy) frames(src, dst net.Conn, last *atomic.Int64, fromServer bool) {
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
		typ, flags := hdr[3], hdr[4]
		stream := binary.BigEndian.Uint32(hdr[5:9]) & 0x7fffffff
		if typ != framePing {
			last.Store(time.Now().UnixNano())
		}
		if fromServer {
			p.note(typ, flags, stream, payload)
		}
		if _, err := dst.Write(append(hdr, payload...)); err != nil {
			return
		}
	}
}

func (p *Proxy) note(typ, flags byte, stream uint32, payload []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case typ == frameSettings && flags&settingsAck == 0:
		for i := 0; i+6 <= len(payload); i += 6 {
			if binary.BigEndian.Uint16(payload[i:]) == settingInitWindow {
				p.streamWindow = binary.BigEndian.Uint32(payload[i+2:])
			}
		}
	case typ == frameWindowUpdate && stream == 0 && p.connIncrement == 0 && len(payload) == 4:
		p.connIncrement = binary.BigEndian.Uint32(payload) & 0x7fffffff
	}
}
