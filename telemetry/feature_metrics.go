package telemetry

import "github.com/prometheus/client_golang/prometheus"

// WebSocket metric labels are bounded by deployment topology or a small
// code-defined set. Never pass endpoints, client or request ids.
var (
	MetricWsConnections = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "ws_connections",
		Help:      "Open WebSocket connections.",
	}, []string{"project", "network"})

	MetricWsSubscriptions = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "ws_subscriptions",
		Help:      "Active eth_subscribe subscriptions, by kind.",
	}, []string{"project", "network", "kind"})

	MetricWsNotificationsTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "ws_notifications_total",
		Help:      "eth_subscription notifications sent to WebSocket clients, by kind.",
	}, []string{"project", "network", "kind"})

	MetricWsClosedTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "ws_connections_closed_total",
		Help:      "WebSocket connections closed, by reason (client, slow_consumer, shutdown, error).",
	}, []string{"project", "network", "reason"})
)
