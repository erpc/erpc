package telemetry

import "github.com/prometheus/client_golang/prometheus"

// Metrics for the opt-in shared block cache, distributed cache fill, leased
// head polling and WebSocket subscriptions. All label values are bounded by
// deployment topology (project, Network.Label()) or by a small code-defined set
// (outcome, kind, reason, op). Never pass endpoints, client or request ids.
var (
	// Distributed cache fill (singleflight across instances).
	MetricCacheFillTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "cache_fill_total",
		Help:      "Distributed cache-fill coordination outcomes (e.g. leader, follower_hit, follower_timeout, error, bypass).",
	}, []string{"project", "network", "outcome"})

	MetricCacheFillWaitSeconds = DefineLabeledHistogram(prometheus.HistogramOpts{
		Namespace: "erpc",
		Name:      "cache_fill_wait_seconds",
		Help:      "Time a follower waited on another instance's cache fill, by outcome.",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"project", "network", "outcome"})

	// Leased upstream head polling.
	MetricHeadPollLeaseHeld = DefineGauge(prometheus.GaugeOpts{
		Namespace: "erpc",
		Name:      "head_poll_lease_held",
		Help:      "1 when this instance holds the distributed head-poll lease for the network, else 0.",
	}, []string{"project", "network"})

	MetricHeadPollSkippedTotal = DefineLabeledCounter(prometheus.CounterOpts{
		Namespace: "erpc",
		Name:      "head_poll_skipped_total",
		Help:      "Upstream head polls skipped because another instance holds the poll lease.",
	}, []string{"project", "network"})

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
