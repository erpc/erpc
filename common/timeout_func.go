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

// NewTimeoutFunc builds a TimeoutFunc from config. The returned function
// is applied at request time via context.WithTimeoutCause.
//
// When Quantile > 0 the timeout is computed per request from method
// latency percentiles; otherwise it returns the fixed Duration.
//
// MinDuration / MaxDuration floor/ceiling the result. **MinDuration
// auto-populates to Duration/2 (or 500ms when Duration is zero) when
// Quantile > 0 and MinDuration is unset** — this prevents the
// feedback-loop bug where success quantiles can drop to 50ms because
// every request fast-fails at 50ms (the previous timeout). See
// specs/failsafe/feature.md §9.4.
func NewTimeoutFunc(logger *zerolog.Logger, cfg *TimeoutPolicyConfig) TimeoutFunc {
	if cfg == nil {
		return nil
	}

	if cfg.Quantile > 0 {
		fixedDur := cfg.Duration.Duration()
		minDur := cfg.MinDuration.Duration()
		maxDur := cfg.MaxDuration.Duration()
		quantile := cfg.Quantile

		// Auto-populate MinDuration floor if unset (prod-bug fix).
		if minDur == 0 {
			if fixedDur > 0 {
				minDur = fixedDur / 2
			} else {
				minDur = 500 * time.Millisecond
			}
		}

		return func(ctx context.Context, req *NormalizedRequest) *time.Duration {
			ntw := req.Network()
			if ntw == nil {
				logger.Debug().Object("request", req).Msg("quantile timeout: no network on request, using fallback")
				return coldStartFallback(fixedDur, maxDur)
			}
			m, _ := req.Method()
			if m == "" {
				logger.Debug().Object("request", req).Msg("quantile timeout: empty method, using fallback")
				return coldStartFallback(fixedDur, maxDur)
			}
			mt := ntw.GetMethodMetrics(m)
			if mt == nil {
				logger.Debug().Object("request", req).Str("method", m).Msg("quantile timeout: no metrics tracker, using fallback")
				return coldStartFallback(fixedDur, maxDur)
			}
			qt := mt.GetResponseQuantiles()
			dr := qt.GetQuantile(quantile)
			if dr <= 0 {
				logger.Debug().Object("request", req).Str("method", m).Msg("quantile timeout: no latency data yet, using fallback")
				return coldStartFallback(fixedDur, maxDur)
			}

			if minDur > 0 && dr < minDur {
				dr = minDur
			}
			if maxDur > 0 && dr > maxDur {
				dr = maxDur
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

	dur := cfg.Duration.Duration()
	if dur == 0 {
		return nil
	}
	return func(_ context.Context, _ *NormalizedRequest) *time.Duration {
		return &dur
	}
}

func coldStartFallback(fixedDur, maxDur time.Duration) *time.Duration {
	fallback := fixedDur
	if fallback == 0 {
		fallback = maxDur
	}
	if fallback > 0 {
		return &fallback
	}
	return nil
}
