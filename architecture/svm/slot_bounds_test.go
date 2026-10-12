package svm

import (
	"context"
	"math"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

type boundsUpstream struct {
	*slotLagUpstream
	cfg *common.UpstreamConfig
}

func (u *boundsUpstream) Config() *common.UpstreamConfig {
	if u.cfg != nil {
		return u.cfg
	}
	return u.slotLagUpstream.Config()
}

func i64(v int64) *int64 { return &v }

func declaredCfg(id string, lower, upper *common.SvmAvailabilityBoundConfig) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:   id,
		Type: common.UpstreamTypeSvm,
		Svm: &common.SvmUpstreamConfig{
			Cluster: "mainnet-beta",
			BlockAvailability: &common.SvmBlockAvailabilityConfig{
				Lower: lower,
				Upper: upper,
			},
		},
	}
}

func upWithBounds(id string, latest, floor int64, floorKnown bool, cfg *common.UpstreamConfig) *boundsUpstream {
	if cfg != nil {
		cfg.Id = id
	}
	return &boundsUpstream{
		slotLagUpstream: &slotLagUpstream{
			id:     id,
			poller: &pollerAtSlot{slot: latest, floor: floor, floorKnown: floorKnown},
		},
		cfg: cfg,
	}
}

func TestResolveDeclaredSlotBounds_Overflow(t *testing.T) {
	t.Parallel()
	// floor + offset can overflow for a valid non-negative config (a huge
	// observed floor plus a large offset). A wrapped lower would silently
	// widen the restriction and a wrapped upper would reject every slot, so
	// overflow resolves as unbounded — the fail-open contract.
	up := upWithBounds("u", 300000, math.MaxInt64-10, true, declaredCfg("u",
		&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: i64(100)},
		&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: i64(100)},
	))
	minB, maxB := ResolveDeclaredSlotBounds(up)
	require.Equal(t, int64(math.MinInt64), minB, "overflowed lower must be unbounded, not wrapped")
	require.Equal(t, int64(math.MaxInt64), maxB, "overflowed upper must be unbounded, not wrapped")

	// Just under the overflow edge the bound still resolves exactly.
	up = upWithBounds("u", math.MaxInt64, math.MaxInt64-100, true, declaredCfg("u",
		&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: i64(100)}, nil,
	))
	minB, _ = ResolveDeclaredSlotBounds(up)
	require.Equal(t, int64(math.MaxInt64), minB)
}

func TestResolveDeclaredSlotBounds(t *testing.T) {
	t.Parallel()

	t.Run("unconfigured is unbounded", func(t *testing.T) {
		up := upWithBounds("u", 300000, 0, true, &common.UpstreamConfig{
			Id: "u", Type: common.UpstreamTypeSvm,
			Svm: &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
		})
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(math.MinInt64), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})

	t.Run("exactSlot is absolute", func(t *testing.T) {
		up := upWithBounds("u", 300000, 0, true, declaredCfg("u",
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(200)},
		))
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(100), minB)
		require.Equal(t, int64(200), maxB)
	})

	t.Run("latestSlotMinus uses poller tip", func(t *testing.T) {
		up := upWithBounds("u", 300000, 0, true, declaredCfg("u",
			&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(100000)},
			nil,
		))
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(200000), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})

	t.Run("firstAvailableSlotPlus uses observed floor including genesis", func(t *testing.T) {
		up := upWithBounds("archive", 300000, 0, true, declaredCfg("archive",
			&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: i64(10)},
			nil,
		))
		minB, _ := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(10), minB, "known genesis floor plus N is 0+N")
	})

	t.Run("firstAvailableSlotPlus with unobserved floor is unbounded", func(t *testing.T) {
		up := upWithBounds("u", 300000, 0, false, declaredCfg("u",
			&common.SvmAvailabilityBoundConfig{FirstAvailableSlotPlus: i64(10)},
			nil,
		))
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(math.MinInt64), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})

	t.Run("latestSlotMinus on a cold poller is unbounded", func(t *testing.T) {
		up := upWithBounds("u", 0, 0, false, declaredCfg("u",
			&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(100000)},
			nil,
		))
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(math.MinInt64), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})

	t.Run("null poller fail-open for relative bounds", func(t *testing.T) {
		up := &boundsUpstream{
			slotLagUpstream: &slotLagUpstream{id: "u", poller: &pollerAtSlot{null: true, slot: 300000, floor: 0, floorKnown: true}},
			cfg: declaredCfg("u",
				&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(100000)},
				nil,
			),
		}
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(math.MinInt64), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})

	t.Run("min greater than max fail-open", func(t *testing.T) {
		// Mixed kinds skip config-time cross-bound validation; at runtime
		// latestSlotMinus:0 is the tip (300000) which is above exactSlot:100.
		up := upWithBounds("u", 300000, 0, true, declaredCfg("u",
			&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(0)},
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		))
		minB, maxB := ResolveDeclaredSlotBounds(up)
		require.Equal(t, int64(math.MinInt64), minB)
		require.Equal(t, int64(math.MaxInt64), maxB)
	})
}

func TestCheckUpstreamDeclaredSlotBounds_ArchiveRestriction(t *testing.T) {
	t.Parallel()
	// Archive at genesis (observed floor 0) plus latestSlotMinus: 100000
	// restricts it to the most recent 100000 slots.
	up := upWithBounds("archive", 300000, 0, true, declaredCfg("archive",
		&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(100000)},
		nil,
	))
	ctx := context.Background()

	err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[1000]`))
	require.Error(t, err, "slot 1000 is below latest-100000")
	require.False(t, retryable)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	require.True(t, common.IsPermanentlyMissingData(err))
	require.Contains(t, err.Error(), "-32001")
	require.Contains(t, err.Error(), "First available block: 200000")

	err, _ = CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[200000]`))
	require.NoError(t, err, "slot at the declared lower is servable")

	err, _ = CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[250000]`))
	require.NoError(t, err)
}

func TestCheckUpstreamDeclaredSlotBounds_FailsOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := declaredCfg("u",
		&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(100000)},
		nil,
	)

	t.Run("cold poller", func(t *testing.T) {
		up := upWithBounds("u", 0, 0, false, cfg)
		err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[1]`))
		require.NoError(t, err)
		require.False(t, retryable)
	})

	t.Run("method out of scope", func(t *testing.T) {
		up := upWithBounds("u", 300000, 0, true, cfg)
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getTransaction", `["sig"]`))
		require.NoError(t, err)
	})
}

func TestCheckUpstreamDeclaredSlotBounds_UpperRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// upper = latest (latestSlotMinus: 0) so anything above the tip is out of window.
	up := upWithBounds("u", 1000, 0, true, declaredCfg("u",
		nil,
		&common.SvmAvailabilityBoundConfig{LatestSlotMinus: i64(0)},
	))

	t.Run("near-tip above upper is retryable", func(t *testing.T) {
		err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[1050]`))
		require.Error(t, err)
		require.True(t, retryable, "50 slots ahead of tip is within the 128-slot retry horizon")
		require.Contains(t, err.Error(), "-32004")
		require.False(t, common.IsPermanentlyMissingData(err))
	})

	t.Run("far above upper is not retryable", func(t *testing.T) {
		err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[1200]`))
		require.Error(t, err)
		require.False(t, retryable, "200 slots ahead exceeds the 128-slot retry horizon")
		require.Contains(t, err.Error(), "-32004")
	})

	t.Run("above a static upper but behind the tip is not retryable", func(t *testing.T) {
		static := upWithBounds("u", 1000, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(500)},
		))
		err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, static, reqOf(t, "getBlock", `[600]`))
		require.Error(t, err)
		require.False(t, retryable, "slot is inside history; waiting cannot raise a configured cap")
	})

	t.Run("slot at upper is servable", func(t *testing.T) {
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up, reqOf(t, "getBlock", `[1000]`))
		require.NoError(t, err)
	})

	t.Run("static upper ahead of the tip is never retryable", func(t *testing.T) {
		// latest=450, exact upper=500, request=550: the tip catching up to 550
		// cannot reopen a cap fixed at 500, so the near-tip retry window must
		// not apply.
		static := upWithBounds("u", 450, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(500)},
		))
		err, retryable := CheckUpstreamDeclaredSlotBounds(ctx, static, reqOf(t, "getBlock", `[550]`))
		require.Error(t, err)
		require.False(t, retryable, "advancing the tip can never move an exact cap")
		require.Contains(t, err.Error(), "-32004")
	})
}

func TestCheckUpstreamDeclaredSlotBounds_RangeEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Window ends at 100. latest=80 for the omitted-end cases.
	up := func() *boundsUpstream {
		return upWithBounds("u", 80, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		))
	}

	// The START is inside the window in every case below; only the END
	// decides the upper verdict.
	t.Run("getBlocks end beyond upper skips", func(t *testing.T) {
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocks", `[50, 1000]`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "-32004")
	})

	t.Run("getBlocks end inside window serves", func(t *testing.T) {
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocks", `[50, 80]`))
		require.NoError(t, err)
	})

	t.Run("getBlocks omitted end extends to tip", func(t *testing.T) {
		// tip 80 <= upper 100: servable.
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocks", `[50]`))
		require.NoError(t, err)
	})

	t.Run("getBlocks omitted end past upper skips", func(t *testing.T) {
		past := upWithBounds("u", 5000, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		))
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, past, reqOf(t, "getBlocks", `[50]`))
		require.Error(t, err, "an omitted end extends to the tip, which is past the window")
		require.Contains(t, err.Error(), "-32004")
	})

	t.Run("getBlocksWithLimit uses tip as upper, beyond window skips", func(t *testing.T) {
		// limit is a block COUNT, not a slot span — end is the upstream tip.
		past := upWithBounds("u", 5000, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		))
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, past, reqOf(t, "getBlocksWithLimit", `[50, 40]`))
		require.Error(t, err, "tip 5000 past upper 100 must skip regardless of limit")
		require.Contains(t, err.Error(), "-32004")
	})

	t.Run("getBlocksWithLimit tip inside window serves", func(t *testing.T) {
		// tip 80 <= upper 100: servable even when start+limit would be past
		// the window under the old (incorrect) span arithmetic.
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocksWithLimit", `[50, 2000]`))
		require.NoError(t, err)
	})

	t.Run("unresolvable end fails open", func(t *testing.T) {
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocks", `[50, "later"]`))
		require.NoError(t, err, "a malformed end is the node's -32602 to answer, not ours")
	})

	t.Run("injected options object reads as omitted end", func(t *testing.T) {
		// Commitment injection rewrites getBlocks(50) to [50, {options}], so a
		// trailing object must take the tip path, not the malformed path.
		// tip 80 <= upper 100: servable.
		err, _ := CheckUpstreamDeclaredSlotBounds(ctx, up(), reqOf(t, "getBlocks", `[50, {"commitment":"confirmed"}]`))
		require.NoError(t, err)

		past := upWithBounds("u", 5000, 0, true, declaredCfg("u",
			nil,
			&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		))
		err, _ = CheckUpstreamDeclaredSlotBounds(ctx, past, reqOf(t, "getBlocks", `[50, {"commitment":"confirmed"}]`))
		require.Error(t, err, "with the options object injected, an omitted end still extends to the tip past the window")
		require.Contains(t, err.Error(), "-32004")
	})
}

func TestCheckUpstreamDeclaredSlotBounds_DoesNotWidenObservedFloor(t *testing.T) {
	t.Parallel()
	// Declared lower 100, observed floor 200: declared check must NOT skip
	// slot 150 (that skip belongs to the floor gate). Declared bounds only restrict.
	up := upWithBounds("u", 300000, 200, true, declaredCfg("u",
		&common.SvmAvailabilityBoundConfig{ExactSlot: i64(100)},
		nil,
	))
	err, _ := CheckUpstreamDeclaredSlotBounds(context.Background(), up, reqOf(t, "getBlock", `[150]`))
	require.NoError(t, err, "declared lower is below the observed floor; the floor gate owns that skip")
}
