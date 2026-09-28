package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Metrics for the opt-in shared block cache and WebSocket subscriptions. All label values are bounded by
// deployment topology (project, Network.Label()) or by a small code-defined set
// (outcome, kind, reason, op). Never pass endpoints, client or request ids.
var (
	// Shared head cache (headcache package).
	MetricHeadCacheRequestsTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_requests_total",
		Help:      "Head cache lookups by result (hit, miss).",
	}, []string{"project", "network", "result"})

	MetricHeadCacheFetchesTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_fetches_total",
		Help:      "Blocks hydrated from upstreams by this instance, by result (ok, rejected).",
	}, []string{"project", "network", "result"})

	MetricHeadCacheReorgsTotal = DefineCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_reorgs_total",
		Help:      "Reorgs (canonical hash mismatches) detected by the head cache leader.",
	}, []string{"project", "network"})

	MetricHeadCachePublishedTotal = DefineCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_snapshots_published_total",
		Help:      "Head cache snapshots published by this instance while leader.",
	}, []string{"project", "network"})

	MetricHeadCacheLeaderEpochsTotal = DefineCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_leader_acquisitions_total",
		Help:      "Times this instance acquired the head cache hydration lease.",
	}, []string{"project", "network"})

	MetricHeadCacheLeader = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_cache_leader",
		Help:      "1 when this instance holds the head cache hydration lease, else 0.",
	}, []string{"project", "network"})

	MetricHeadCacheHead = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_cache_head_block",
		Help:      "Head block number of the snapshot currently held by the head cache.",
	}, []string{"project", "network"})

	MetricHeadCacheBytes = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_cache_bytes",
		Help:      "Bytes of block records held in memory by the head cache.",
	}, []string{"project", "network"})

	MetricHeadCacheSnapshotTimestamp = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_cache_snapshot_timestamp_seconds",
		Help:      "Unix time the currently held snapshot was published by the leader (freshness = time() - value).",
	}, []string{"project", "network"})

	MetricHeadCacheSubscribers = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_cache_subscribers",
		Help:      "Active in-process head cache subscriptions.",
	}, []string{"project", "network"})

	MetricHeadCacheSubscriberClosedTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_cache_subscriber_closed_total",
		Help:      "Head cache subscriptions terminated, by reason (unsubscribe, slow_consumer, gap, reset, stop).",
	}, []string{"project", "network", "reason"})

	// WebSocket server.
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
