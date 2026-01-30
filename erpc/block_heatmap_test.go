package erpc

import (
	"strings"
	"testing"
)

func TestComputeBucket_ComprehensiveWithRealBlockNumbers(t *testing.T) {
	t.Parallel()
	tip := int64(125_237_247)

	cases := []struct {
		name          string
		blockNumber   int64
		expectedStart int64
		expectedEnd   int64
		expectedSize  int64
		expectedLabel string
	}{
		// Within last 100 blocks (size=100)
		{
			name:          "exact_tip",
			blockNumber:   125_237_247,
			expectedStart: 125_237_200,
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "TIP",
		},
		{
			name:          "4_blocks_before_tip_within_tolerance",
			blockNumber:   125_237_243,
			expectedStart: 125_237_200,
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "TIP", // Within ±4 blocks tolerance
		},
		{
			name:          "5_blocks_before_tip_outside_tolerance",
			blockNumber:   125_237_242,
			expectedStart: 125_237_200,
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "L100", // Outside ±4 blocks tolerance
		},

		// Within last 500 blocks (size=100 because distance is 400)
		{
			name:          "400_blocks_before_tip",
			blockNumber:   125_236_847,
			expectedStart: 125_236_800,
			expectedEnd:   125_236_900,
			expectedSize:  100,
			expectedLabel: "L400-L300", // 447 rounds to 400, 347 rounds to 300
		},

		// Within last 5k blocks (size=1000 because distance is 1500)
		{
			name:          "1500_blocks_before_tip",
			blockNumber:   125_235_747,
			expectedStart: 125_235_000,
			expectedEnd:   125_236_000,
			expectedSize:  1_000,
			expectedLabel: "L2k-L1k", // 2247 rounds to 2k, 1247 rounds to 1k (multiples of 1k)
		},

		// Within last 5k blocks but distance is exactly 2k (size=1000)
		{
			name:          "2k_blocks_before_tip",
			blockNumber:   125_235_247,
			expectedStart: 125_235_000,
			expectedEnd:   125_236_000,
			expectedSize:  1_000,
			expectedLabel: "L2k-L1k", // 2247 rounds to 2k, 1247 rounds to 1k (multiples of 1k)
		},

		// Within last 10k blocks (size=5k)
		{
			name:          "10k_blocks_before_tip",
			blockNumber:   125_227_247,
			expectedStart: 125_225_000,
			expectedEnd:   125_230_000,
			expectedSize:  5_000,
			expectedLabel: "L10k-L5k", // 12247 rounds to 10k, 7247 rounds to 5k (multiples of 5k)
		},

		// Within last 50k blocks (size=10k)
		{
			name:          "30k_blocks_before_tip",
			blockNumber:   125_207_247,
			expectedStart: 125_200_000,
			expectedEnd:   125_210_000,
			expectedSize:  10_000,
			expectedLabel: "L40k-L30k", // 37247 rounds to 40k, 27247 rounds to 30k (multiples of 10k)
		},

		// Within last 100k blocks (size=30k)
		{
			name:          "80k_blocks_before_tip",
			blockNumber:   125_157_247,
			expectedStart: 125_130_000, // 125157247 / 30000 * 30000 = 125130000
			expectedEnd:   125_160_000, // 125130000 + 30000 = 125160000
			expectedSize:  30_000,
			expectedLabel: "L120k-L90k", // 107247 rounds to 120k, 77247 rounds to 90k (multiples of 30k)
		},

		// Within last 300k blocks (size=50k)
		{
			name:          "200k_blocks_before_tip",
			blockNumber:   125_037_247,
			expectedStart: 125_000_000,
			expectedEnd:   125_050_000,
			expectedSize:  50_000,
			expectedLabel: "L250k-L200k", // 237247 rounds to 250k, 187247 rounds to 200k (multiples of 50k)
		},

		// Within last 1m blocks (size=100k)
		{
			name:          "500k_blocks_before_tip",
			blockNumber:   124_737_247,
			expectedStart: 124_700_000,
			expectedEnd:   124_800_000,
			expectedSize:  100_000,
			expectedLabel: "L500k-L400k", // 537247 rounds to 500k, 437247 rounds to 400k (multiples of 100k)
		},

		// Within last 5m blocks (size=1m)
		{
			name:          "2m_blocks_before_tip",
			blockNumber:   123_237_247,
			expectedStart: 123_000_000,
			expectedEnd:   124_000_000,
			expectedSize:  1_000_000,
			expectedLabel: "L2m-L1m",
		},

		// Just beyond 5m blocks - should use absolute labels
		{
			name:          "6m_blocks_before_tip",
			blockNumber:   119_237_247,
			expectedStart: 119_000_000,
			expectedEnd:   120_000_000,
			expectedSize:  1_000_000,
			expectedLabel: "119m-120m",
		},

		// Far from tip
		{
			name:          "10m_blocks_before_tip",
			blockNumber:   115_237_247,
			expectedStart: 115_000_000,
			expectedEnd:   116_000_000,
			expectedSize:  1_000_000,
			expectedLabel: "115m-116m",
		},

		// Blocks ahead of tip
		{
			name:          "3_blocks_ahead_within_tolerance",
			blockNumber:   125_237_250,
			expectedStart: 125_237_200, // Should align to 100 boundary
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "TIP", // Within ±4 blocks tolerance
		},
		{
			name:          "5_blocks_ahead_outside_tolerance",
			blockNumber:   125_237_252,
			expectedStart: 125_237_200, // Should align to 100 boundary
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "FUTURE", // Outside ±4 blocks tolerance
		},
		{
			name:          "far_ahead_of_tip",
			blockNumber:   125_237_300,
			expectedStart: 125_237_200, // Should align to 100 boundary
			expectedEnd:   125_237_247,
			expectedSize:  100,
			expectedLabel: "FUTURE",
		},

		// Edge case: exactly at 1k boundary (size=100)
		{
			name:          "exactly_1k_before_tip",
			blockNumber:   125_236_247,
			expectedStart: 125_236_200,
			expectedEnd:   125_236_300,
			expectedSize:  100,
			expectedLabel: "L1k-L900", // 1047 rounds to 1k, 947 rounds to 900 (multiples of 100)
		},

		// Edge case: exactly at 5k boundary (size=1000)
		{
			name:          "exactly_5k_before_tip",
			blockNumber:   125_232_247,
			expectedStart: 125_232_000,
			expectedEnd:   125_233_000,
			expectedSize:  1_000,
			expectedLabel: "L5k-L4k", // 5247 rounds to 5k, 4247 rounds to 4k
		},

		// Edge case: exactly at 20k boundary (size=5000)
		{
			name:          "exactly_20k_before_tip",
			blockNumber:   125_217_247,
			expectedStart: 125_215_000,
			expectedEnd:   125_220_000,
			expectedSize:  5_000,
			expectedLabel: "L20k-L15k", // 22247 rounds to 20k, 17247 rounds to 15k
		},

		// Edge case: exactly at 50k boundary (size=10000)
		{
			name:          "exactly_50k_before_tip",
			blockNumber:   125_187_247,
			expectedStart: 125_180_000,
			expectedEnd:   125_190_000,
			expectedSize:  10_000,
			expectedLabel: "L60k-L50k", // 57247 rounds to 60k, 47247 rounds to 50k
		},

		// Edge case: exactly at 100k boundary (size=30000)
		{
			name:          "exactly_100k_before_tip",
			blockNumber:   125_137_247,
			expectedStart: 125_130_000, // 125137247 / 30000 * 30000 = 125130000
			expectedEnd:   125_160_000, // 125130000 + 30000 = 125160000
			expectedSize:  30_000,
			expectedLabel: "L120k-L90k", // 107247 rounds to 120k, 77247 rounds to 90k (multiples of 30k)
		},

		// Edge case: exactly at 300k boundary (size=50000)
		{
			name:          "exactly_300k_before_tip",
			blockNumber:   124_937_247,
			expectedStart: 124_900_000,
			expectedEnd:   124_950_000,
			expectedSize:  50_000,
			expectedLabel: "L350k-L300k", // 337247 rounds to 350k, 287247 rounds to 300k
		},

		// Edge case: exactly at 1m boundary (size=100000)
		{
			name:          "exactly_1m_before_tip",
			blockNumber:   124_237_247,
			expectedStart: 124_200_000,
			expectedEnd:   124_300_000,
			expectedSize:  100_000,
			expectedLabel: "L1m-L900k", // 1037247 rounds to 1m, 937247 rounds to 900k (multiples of 100k)
		},

		// Edge case: bucket that ends very close to tip (should not show L100-L0)
		{
			name:          "bucket_almost_at_tip",
			blockNumber:   125_237_145,
			expectedStart: 125_237_100,
			expectedEnd:   125_237_200,
			expectedSize:  100,
			expectedLabel: "L100", // Should be L100, not L100-L0 since end is so close to tip
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, size, label := ComputeBlockHeatmapBucket(tc.blockNumber, tip, "")

			if start != tc.expectedStart {
				t.Errorf("start: got %d, want %d", start, tc.expectedStart)
			}
			if end != tc.expectedEnd {
				t.Errorf("end: got %d, want %d", end, tc.expectedEnd)
			}
			if size != tc.expectedSize {
				t.Errorf("size: got %d, want %d", size, tc.expectedSize)
			}
			if label != tc.expectedLabel {
				t.Errorf("label: got %s, want %s", label, tc.expectedLabel)
			}

			// Additional validation
			if size > 0 {
				if start%size != 0 {
					t.Errorf("start %d is not aligned to size %d", start, size)
				}
				// Check that the label doesn't have decimals
				for i, c := range label {
					if c == '.' {
						t.Errorf("label contains decimal point at position %d: %s", i, label)
					}
				}
			}
		})
	}
}

func TestComputeBucket_TipTolerance(t *testing.T) {
	t.Parallel()
	tip := int64(100000)

	// Test the ±4 blocks tolerance for TIP label
	cases := []struct {
		name          string
		blockNumber   int64
		expectedLabel string
	}{
		// Exact tip
		{
			name:          "exact_tip",
			blockNumber:   100000,
			expectedLabel: "TIP",
		},
		// Within tolerance before tip
		{
			name:          "1_block_before",
			blockNumber:   99999,
			expectedLabel: "TIP",
		},
		{
			name:          "4_blocks_before",
			blockNumber:   99996,
			expectedLabel: "TIP",
		},
		{
			name:          "5_blocks_before",
			blockNumber:   99995,
			expectedLabel: "L100", // Outside tolerance
		},
		// Within tolerance after tip
		{
			name:          "1_block_ahead",
			blockNumber:   100001,
			expectedLabel: "TIP",
		},
		{
			name:          "4_blocks_ahead",
			blockNumber:   100004,
			expectedLabel: "TIP",
		},
		{
			name:          "5_blocks_ahead",
			blockNumber:   100005,
			expectedLabel: "FUTURE", // Outside tolerance
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, label := ComputeBlockHeatmapBucket(tc.blockNumber, tip, "")

			if label != tc.expectedLabel {
				t.Errorf("label: got %s, want %s", label, tc.expectedLabel)
			}
		})
	}
}

func TestComputeBucket_NoL0Labels(t *testing.T) {
	t.Parallel()
	tip := int64(10000)

	// Test various distances that might produce L<x>-L0 labels
	cases := []struct {
		name          string
		blockNumber   int64
		expectedLabel string
	}{
		{
			name:          "99_blocks_before_tip",
			blockNumber:   9901,
			expectedLabel: "L100", // Not L100-L0
		},
		{
			name:          "51_blocks_before_tip",
			blockNumber:   9949,
			expectedLabel: "L100", // Not L100-L0
		},
		{
			name:          "1_block_before_tip",
			blockNumber:   9999,
			expectedLabel: "TIP", // Within ±4 blocks tolerance
		},
		{
			name:          "999_blocks_before_tip",
			blockNumber:   9001,
			expectedLabel: "L1k-L900", // Bucket is 9000-9100, which is 1k-900 from tip
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, label := ComputeBlockHeatmapBucket(tc.blockNumber, tip, "")

			if label != tc.expectedLabel {
				t.Errorf("label: got %s, want %s", label, tc.expectedLabel)
			}

			// Also verify no "-L0" suffix
			if strings.Contains(label, "-L0") {
				t.Errorf("label should not contain '-L0': %s", label)
			}
		})
	}
}

func TestComputeBucket_WithBlockReferences(t *testing.T) {
	t.Parallel()
	tip := int64(125_237_247)

	cases := []struct {
		name          string
		blockNumber   int64
		blockRef      string
		expectedLabel string
	}{
		{
			name:          "latest_tag",
			blockNumber:   125_237_247,
			blockRef:      "latest",
			expectedLabel: "LATEST",
		},
		{
			name:          "finalized_tag",
			blockNumber:   125_237_000,
			blockRef:      "finalized",
			expectedLabel: "FINALIZED",
		},
		{
			name:          "safe_tag",
			blockNumber:   125_237_100,
			blockRef:      "safe",
			expectedLabel: "SAFE",
		},
		{
			name:          "pending_tag",
			blockNumber:   125_237_300,
			blockRef:      "pending",
			expectedLabel: "PENDING",
		},
		{
			name:          "numeric_block_ref_no_special_label",
			blockNumber:   125_237_247,
			blockRef:      "0x1234567",
			expectedLabel: "TIP", // Falls back to TIP since it's at the tip
		},
		{
			name:          "empty_ref_with_tip",
			blockNumber:   125_237_247,
			blockRef:      "",
			expectedLabel: "TIP",
		},
		{
			name:          "latest_tag_but_not_at_tip",
			blockNumber:   125_237_200,
			blockRef:      "latest",
			expectedLabel: "LATEST", // Still uses LATEST even if not at exact tip
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, label := ComputeBlockHeatmapBucket(tc.blockNumber, tip, tc.blockRef)

			if label != tc.expectedLabel {
				t.Errorf("label: got %s, want %s", label, tc.expectedLabel)
			}
		})
	}
}
