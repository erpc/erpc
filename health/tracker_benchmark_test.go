package health

import (
	"context"
	"io"
	"math/rand"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// upstreamStub implements common.Upstream with only the methods we use in benchmarks.
type upstreamStub struct {
	id           string
	vendor       string
	networkId    string
	networkLabel string
	logger       *zerolog.Logger
}

func (u *upstreamStub) Id() string                     { return u.id }
func (u *upstreamStub) VendorName() string             { return u.vendor }
func (u *upstreamStub) NetworkId() string              { return u.networkId }
func (u *upstreamStub) NetworkLabel() string           { return u.networkLabel }
func (u *upstreamStub) Config() *common.UpstreamConfig { return nil }
func (u *upstreamStub) Logger() *zerolog.Logger        { return u.logger }
func (u *upstreamStub) Vendor() common.Vendor          { return nil }
func (u *upstreamStub) Tracker() common.HealthTracker  { return nil }
func (u *upstreamStub) Forward(_ context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *upstreamStub) Cordon(string, string)   {}
func (u *upstreamStub) Uncordon(string, string) {}
func (u *upstreamStub) IgnoreMethod(string)     {}

// labelCombo captures the full label set for MetricUpstreamRequestDuration
type labelCombo struct {
	up     *upstreamStub
	method string
	comp   string
	final  common.DataFinalityState
	user   string
}

func buildCombos(project string, upstreams int, methods []string, users []string) ([]labelCombo, []*upstreamStub) {
	lg := zerolog.New(io.Discard)
	us := make([]*upstreamStub, 0, upstreams)
	for i := 0; i < upstreams; i++ {
		us = append(us, &upstreamStub{
			id:           "ups-" + strconv.Itoa(i),
			vendor:       "vendor" + strconv.Itoa(i%5),
			networkId:    "net-" + strconv.Itoa(i%10),
			networkLabel: "netlbl-" + strconv.Itoa(i%10),
			logger:       &lg,
		})
	}
	finalities := []common.DataFinalityState{common.DataFinalityStateRealtime, common.DataFinalityStateUnfinalized, common.DataFinalityStateFinalized}
	comps := []string{"none"}
	combos := make([]labelCombo, 0, upstreams*len(methods)*len(users))
	for _, u := range us {
		for _, m := range methods {
			for _, user := range users {
				combos = append(combos, labelCombo{up: u, method: m, comp: comps[0], final: finalities[rand.Intn(len(finalities))], user: user})
			}
		}
	}
	return combos, us
}

func prewarmPerCall(combos []labelCombo, project string) {
	for _, c := range combos {
		telemetry.MetricUpstreamRequestDuration.
			WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, c.comp, c.final.String(), c.user).
			Observe(0.001)
	}
}

func prewarmCached(tk *Tracker, combos []labelCombo) {
	for _, c := range combos {
		_ = tk.getUpstreamRequestDurationObserver(c.up, c.method, c.comp, c.final, c.user)
	}
}

func initBenchMetrics(b *testing.B) {
	// Ensure histogram vectors are initialized and registry is clean between runs
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	if err := telemetry.SetHistogramBuckets(""); err != nil {
		b.Fatalf("failed to init histogram buckets: %v", err)
	}
}

func BenchmarkMetricUpstreamRequestDuration_PerCall(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)
	prewarmPerCall(combos, project)
	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		for pb.Next() {
			c := combos[idx]
			telemetry.MetricUpstreamRequestDuration.
				WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, c.comp, c.final.String(), c.user).
				Observe(0.003)
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

func BenchmarkMetricUpstreamRequestDuration_Cached(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)

	lg := zerolog.New(io.Discard)
	tk := NewTracker(&lg, project, time.Minute)
	prewarmCached(tk, combos)

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		for pb.Next() {
			c := combos[idx]
			// isSuccess=false to avoid quantile updates and isolate the histogram path cost
			tk.RecordUpstreamDuration(c.up, c.method, 3*time.Millisecond, false, c.comp, c.final, c.user)
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

func prewarmPerCallSelfRateLimited(combos []labelCombo, project string) {
	for _, c := range combos {
		telemetry.MetricRateLimitsTotal.
			WithLabelValues(project, c.up.networkLabel, c.up.vendor, c.up.id, c.method, "", "n/a", "unknown", "budget-x", "upstream", "auth:0", "upstream").
			Inc()
	}
}

func prewarmPerCallRemoteRateLimited(combos []labelCombo, project string) {
	for _, c := range combos {
		telemetry.MetricRateLimitsTotal.
			WithLabelValues(project, c.up.networkLabel, c.up.vendor, c.up.id, c.method, "", "n/a", "unknown", "<remote>", "remote", "", "upstream").
			Inc()
	}
}

func BenchmarkUpstreamSelfRateLimited_PerCall(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)
	prewarmPerCallSelfRateLimited(combos, project)

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		for pb.Next() {
			c := combos[idx]
			telemetry.MetricRateLimitsTotal.
				WithLabelValues(project, c.up.networkLabel, c.up.vendor, c.up.id, c.method, "", "n/a", "unknown", "budget-x", "upstream", "auth:0", "upstream").
				Inc()
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

func BenchmarkUpstreamSelfRateLimited_Cached(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)

	lg := zerolog.New(io.Discard)
	tk := NewTracker(&lg, project, time.Minute)
	// Prewarm by exercising the public API; upstream self counter handle was removed
	for _, c := range combos {
		tk.RecordUpstreamSelfRateLimited(c.up, c.method, nil)
	}

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		for pb.Next() {
			c := combos[idx]
			tk.RecordUpstreamSelfRateLimited(c.up, c.method, nil)
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

func BenchmarkUpstreamRemoteRateLimited_PerCall(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)
	prewarmPerCallRemoteRateLimited(combos, project)

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		for pb.Next() {
			c := combos[idx]
			telemetry.MetricRateLimitsTotal.
				WithLabelValues(project, c.up.networkLabel, c.up.vendor, c.up.id, c.method, "", "n/a", "unknown", "<remote>", "remote", "", "upstream").
				Inc()
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

func BenchmarkUpstreamRemoteRateLimited_Cached(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)

	lg := zerolog.New(io.Discard)
	tk := NewTracker(&lg, project, time.Minute)
	// Prewarm cached handles
	for _, c := range combos {
		tk.getRemoteRateLimitedCounter(c.up, c.method, "n/a", "unknown", "unknown")
	}

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		idx := local.Intn(len(combos))
		ctx := context.Background()
		for pb.Next() {
			c := combos[idx]
			tk.RecordUpstreamRemoteRateLimited(ctx, c.up, c.method, nil)
			idx++
			if idx >= len(combos) {
				idx = 0
			}
		}
	})
}

// Baseline per-call benches for request and error counters (no cached variants yet)
func prewarmPerCallRequestCounter(combos []labelCombo, project string) {
	attempts := []string{"1", "2", "3"}
	for _, c := range combos {
		for _, a := range attempts {
			telemetry.MetricUpstreamRequestTotal.
				WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, a, c.comp, c.final.String(), c.user, "unknown").
				Inc()
		}
	}
}

func prewarmPerCallErrorCounter(combos []labelCombo, project string) {
	errors := []string{"errA", "errB"}
	severities := []string{"minor", "major"}
	for _, c := range combos {
		for _, e := range errors {
			for _, s := range severities {
				telemetry.MetricUpstreamErrorTotal.
					WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, e, s, c.comp, c.final.String(), c.user, "unknown").
					Inc()
			}
		}
	}
}

func BenchmarkUpstreamRequestTotal_PerCall(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)
	prewarmPerCallRequestCounter(combos, project)

	attempts := []string{"1", "2", "3"}
	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		idx := rand.Intn(len(combos))
		ai := rand.Intn(len(attempts))
		for pb.Next() {
			c := combos[idx]
			a := attempts[ai]
			telemetry.MetricUpstreamRequestTotal.
				WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, a, c.comp, c.final.String(), c.user, "unknown").
				Inc()
			idx++
			if idx >= len(combos) {
				idx = 0
			}
			ai++
			if ai >= len(attempts) {
				ai = 0
			}
		}
	})
}

func BenchmarkUpstreamErrorTotal_PerCall(b *testing.B) {
	initBenchMetrics(b)
	project := "bench"
	upstreams := 200
	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	users := []string{"u1", "u2", "u3"}
	combos, _ := buildCombos(project, upstreams, methods, users)
	prewarmPerCallErrorCounter(combos, project)

	errors := []string{"errA", "errB"}
	severities := []string{"minor", "major"}
	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		idx := rand.Intn(len(combos))
		ei := rand.Intn(len(errors))
		si := rand.Intn(len(severities))
		for pb.Next() {
			c := combos[idx]
			e := errors[ei]
			s := severities[si]
			telemetry.MetricUpstreamErrorTotal.
				WithLabelValues(project, c.up.vendor, c.up.networkLabel, c.up.id, c.method, e, s, c.comp, c.final.String(), c.user, "unknown").
				Inc()
			idx++
			if idx >= len(combos) {
				idx = 0
			}
			ei++
			if ei >= len(errors) {
				ei = 0
			}
			si++
			if si >= len(severities) {
				si = 0
			}
		}
	})
}
