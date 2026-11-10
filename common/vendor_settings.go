package common

import (
	"fmt"
	"math"
	"time"
)

// RecheckInterval reads the catalog freshness window from provider settings.
// Numeric config values are milliseconds; programmatic durations remain
// nanoseconds. Only an absent or null value uses the vendor's default.
func (s VendorSettings) RecheckInterval(defaultInterval time.Duration) (time.Duration, error) {
	value := s["recheckInterval"]
	if value == nil {
		return defaultInterval, nil
	}

	var milliseconds int64
	switch v := value.(type) {
	case time.Duration:
		return v, nil
	case Duration:
		return v.Duration(), nil
	case string:
		interval, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("settings.recheckInterval: %w", err)
		}
		return interval, nil
	case int:
		milliseconds = int64(v)
	case int64:
		milliseconds = v
	case uint64:
		if v > uint64(math.MaxInt64/int64(time.Millisecond)) {
			return 0, fmt.Errorf("settings.recheckInterval: milliseconds exceed time.Duration range")
		}
		milliseconds = int64(v)
	case float64:
		nanoseconds := v * float64(time.Millisecond)
		// The upper bound is exclusive: float64(MaxInt64) rounds to 1<<63.
		if math.IsNaN(nanoseconds) || nanoseconds >= float64(math.MaxInt64) || nanoseconds < float64(math.MinInt64) {
			return 0, fmt.Errorf("settings.recheckInterval: milliseconds must be finite and within time.Duration range")
		}
		return time.Duration(nanoseconds), nil
	default:
		return 0, fmt.Errorf("settings.recheckInterval: expected a duration string or number of milliseconds, got %T", value)
	}
	if milliseconds > math.MaxInt64/int64(time.Millisecond) || milliseconds < math.MinInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("settings.recheckInterval: milliseconds exceed time.Duration range")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}
