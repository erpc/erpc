package erpc

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEligibleLane covers the block-availability "lane" decision that feeds
// the selection policy's per-boundary axis. The interesting cases are the two
// "no scoping → nil" collapses (no bounds anywhere; every upstream eligible),
// which keep the engine from spawning a lane slot identical to the wildcard.
func TestEligibleLane(t *testing.T) {
	const (
		lo = math.MinInt64
		hi = math.MaxInt64
	)
	// archive: fully unbounded. recent: only blocks >= 100 (lower.exactBlock).
	// capped: only blocks <= 200 (an upper bound). window: [100,200].
	archive := upstreamBlockBounds{id: "archive", min: lo, max: hi}
	recent := upstreamBlockBounds{id: "recent", min: 100, max: hi}
	capped := upstreamBlockBounds{id: "capped", min: lo, max: 200}
	window := upstreamBlockBounds{id: "window", min: 100, max: 200}

	cases := []struct {
		name   string
		bounds []upstreamBlockBounds
		bn     int64
		want   []string // nil means "no lane scoping"
	}{
		{
			name:   "no bounds anywhere → nil",
			bounds: []upstreamBlockBounds{{id: "a", min: lo, max: hi}, {id: "b", min: lo, max: hi}},
			bn:     5,
			want:   nil,
		},
		{
			name:   "historical block excludes recent-only node → proper subset",
			bounds: []upstreamBlockBounds{archive, recent},
			bn:     1,
			want:   []string{"archive"},
		},
		{
			name:   "in-range block: all eligible → nil",
			bounds: []upstreamBlockBounds{archive, recent},
			bn:     100,
			want:   nil,
		},
		{
			name:   "above an upper bound excludes the capped node",
			bounds: []upstreamBlockBounds{archive, capped},
			bn:     250,
			want:   []string{"archive"},
		},
		{
			name:   "windowed node included only inside its window",
			bounds: []upstreamBlockBounds{archive, window},
			bn:     150,
			want:   nil, // both eligible inside the window
		},
		{
			name:   "windowed node excluded below its window",
			bounds: []upstreamBlockBounds{archive, window},
			bn:     50,
			want:   []string{"archive"},
		},
		{
			name:   "no upstream can serve → empty (non-nil) lane",
			bounds: []upstreamBlockBounds{recent},
			bn:     1,
			want:   []string{}, // recent is bounded and excluded → empty, NOT the full-pool nil
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eligibleLane(tc.bounds, tc.bn)
			if tc.want == nil {
				require.Nil(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}
}
