package common

import (
	"context"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// TimeoutFunc computes the timeout for a request. Returns nil when no
// timeout applies (caller skips context.WithTimeout).
type TimeoutFunc func(ctx context.Context, req *NormalizedRequest) *time.Duration

// NewTimeoutFunc builds a TimeoutFunc from config. The timeout is a
// AdaptiveDuration: a Base (static fallback) plus an optional Quantile that
// pulls the cap from the per-method latency tracker, clamped by Min/Max.
//
// When Quantile > 0 but Min is unset, Min auto-populates to Base/2 (or
// 500ms when Base is also zero) — this prevents the feedback-loop bug
// where success quantiles can collapse to 50ms because every request
// fast-fails at 50ms (the previous timeout).
func NewTimeoutFunc(logger *zerolog.Logger, cfg *TimeoutPolicyConfig) TimeoutFunc {
	if cfg == nil || cfg.Duration.IsZero() {
		return nil
	}
	spec := cfg.Duration

	// Apply the auto-floor only when Quantile-driven and Min is unset.
	resolved := *spec
	if resolved.Quantile > 0 && resolved.Min == 0 {
		if resolved.Base > 0 {
			resolved.Min = Duration(resolved.Base.Duration() / 2)
		} else {
			resolved.Min = Duration(500 * time.Millisecond)
		}
	}

	if resolved.Quantile <= 0 {
		dur := resolved.Resolve(nil)
		if dur <= 0 {
			return nil
		}
		return func(_ context.Context, _ *NormalizedRequest) *time.Duration {
			return &dur
		}
	}

	return func(ctx context.Context, req *NormalizedRequest) *time.Duration {
		ntw := req.Network()
		if ntw == nil {
			logger.Debug().Object("request", req).Msg("quantile timeout: no network on request, using fallback")
			return coldStartFallback(&resolved)
		}
		m, _ := req.Method()
		if m == "" {
			logger.Debug().Object("request", req).Msg("quantile timeout: empty method, using fallback")
			return coldStartFallback(&resolved)
		}
		mt := ntw.GetMethodMetrics(m)
		if mt == nil {
			logger.Debug().Object("request", req).Str("method", m).Msg("quantile timeout: no metrics tracker, using fallback")
			return coldStartFallback(&resolved)
		}
		qt := mt.GetResponseQuantiles()
		dr := resolved.Resolve(qt)
		if dr <= 0 {
			logger.Debug().Object("request", req).Str("method", m).Msg("quantile timeout: no latency data yet, using fallback")
			return coldStartFallback(&resolved)
		}

		finality := req.Finality(ctx)
		telemetry.ObserverHandle(
			telemetry.MetricNetworkTimeoutDurationSeconds,
			ntw.ProjectId(),
			req.NetworkLabel(),
			m,
			finality.String(),
		).Observe(dr.Seconds())
		logger.Trace().Object("request", req).Dur("timeout", dr).Msgf("calculated dynamic timeout")
		return &dr
	}
}

func coldStartFallback(spec *AdaptiveDuration) *time.Duration {
	fallback := spec.Base.Duration()
	if fallback == 0 {
		fallback = spec.Max.Duration()
	}
	if fallback > 0 {
		return &fallback
	}
	return nil
}
