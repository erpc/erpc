package util

import (
	"net"
	"time"
)

// DefaultOutboundDialer returns a net.Dialer configured with kernel-level
// TCP keepalive at 15s intervals. Without explicit KeepAlive, Go falls
// back to the OS default tcp_keepalive_time (7200s on Linux), so wedged
// outbound TCP flows are invisible to kernel-level cleanup for two hours.
// With 15s probes, a flow that loses connectivity is killed in ~45s
// (3 missed probes), which lets application-level timeouts fire cleanly.
//
// This matches what Go's own http.DefaultTransport does
// (KeepAlive=30s) and is the standard pattern for production outbound
// HTTP clients. Use as the DialContext source for any http.Transport
// you build by hand.
func DefaultOutboundDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
	}
}
