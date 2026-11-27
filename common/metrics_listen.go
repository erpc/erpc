package common

import "fmt"

// ListenAddr is the TCP address the /metrics server binds to.
//
// metrics.port is shared between the two families, so only one listener can be
// created: IPv4 is preferred, and IPv6 is consulted when there is no usable
// IPv4 address — because listenV4 is off, or because hostV4 is unset and only
// hostV6 is configured. Both listen flags left nil keeps every config written
// before these keys were honoured binding where it used to (hostV4, which
// defaults to 0.0.0.0).
//
// Returning the address rather than an open listener keeps the decision
// testable without a socket.
func (m *MetricsConfig) ListenAddr() (string, error) {
	if m == nil {
		return "", fmt.Errorf("metrics config is missing")
	}
	if m.Port == nil {
		return "", fmt.Errorf("metrics.port is required when metrics.enabled is true")
	}

	wantV4 := m.ListenV4 == nil || *m.ListenV4
	wantV6 := m.ListenV6 == nil || *m.ListenV6

	if wantV4 && m.HostV4 != nil {
		return fmt.Sprintf("%s:%d", *m.HostV4, *m.Port), nil
	}
	if wantV6 && m.HostV6 != nil {
		return fmt.Sprintf("%s:%d", *m.HostV6, *m.Port), nil
	}

	// Nothing is left to bind: say which key made it impossible, so the fix is
	// obvious from the startup error alone.
	if m.ListenV4 != nil && *m.ListenV4 && m.HostV4 == nil {
		return "", fmt.Errorf("metrics.listenV4 is true but metrics.hostV4 is not set")
	}
	if m.ListenV6 != nil && *m.ListenV6 && m.HostV6 == nil {
		return "", fmt.Errorf("metrics.listenV6 is true but metrics.hostV6 is not set")
	}
	return "", fmt.Errorf("you must configure at least one of metrics.listenV4 or metrics.listenV6")
}
