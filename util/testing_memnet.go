package util

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// A server from httptest.NewTestServer serves an in-memory network that only
// its own Client reaches. RouteInMemory gives the server a host name, and
// DialInMemory dials that name, so code that dials a URL itself (a WebSocket
// client, for example) also reaches the server.
var (
	inMemoryMu    sync.Mutex
	inMemoryDials = map[string]func(context.Context, string, string) (net.Conn, error){}
	inMemorySeq   atomic.Uint64
)

// RouteInMemory starts srv, a server from httptest.NewTestServer, and sets
// srv.URL to a host name that DialInMemory resolves to srv. Configure srv
// before the call.
func RouteInMemory(t testing.TB, srv *httptest.Server) {
	t.Helper()
	dial := srv.Client().Transport.(*http.Transport).DialContext
	host := fmt.Sprintf("srv%d.test", inMemorySeq.Add(1))
	inMemoryMu.Lock()
	inMemoryDials[host] = dial
	inMemoryMu.Unlock()
	t.Cleanup(func() {
		inMemoryMu.Lock()
		delete(inMemoryDials, host)
		inMemoryMu.Unlock()
	})
	srv.URL = "http://" + host
}

// DialInMemory dials a server that RouteInMemory made reachable, or any other
// address on the network.
func DialInMemory(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	inMemoryMu.Lock()
	dial := inMemoryDials[host]
	inMemoryMu.Unlock()
	if dial == nil {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return dial(ctx, network, addr)
}
