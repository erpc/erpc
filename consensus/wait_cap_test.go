package consensus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWaitCap_MaxWaitOnResult_BoundsTailLatency verifies that once one
// non-empty response arrives, the analyzer resolves within maxWaitOnResult
// even if a sibling participant is still running.
func TestWaitCap_MaxWaitOnResult_BoundsTailLatency(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(3). // require 3 to disable short-circuit
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnResult(100 * time.Millisecond).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		switch idx {
		case 1:
			// fast non-empty response — arms maxWaitOnResult deadline
			return validResponseWithValue("0xfast"), nil
		case 2:
			// medium speed (well within cap) — also non-empty
			time.Sleep(20 * time.Millisecond)
			return validResponseWithValue("0xfast"), nil
		default:
			// slow straggler — exceeds the cap, should be cancelled
			time.Sleep(2 * time.Second)
			return validResponseWithValue("0xslow"), nil
		}
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Less(t, elapsed, 800*time.Millisecond,
		"wait cap must bound elapsed time well below the straggler's 2s")
}

// TestWaitCap_MaxWaitOnEmpty_TighterFloor verifies that when ONLY empty
// responses have arrived, the (typically larger) maxWaitOnEmpty cap
// applies — bounded even if no real answer is ever produced.
func TestWaitCap_MaxWaitOnEmpty_TighterFloor(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnEmpty(150 * time.Millisecond).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		switch idx {
		case 1:
			// first response is empty — arms maxWaitOnEmpty
			return validResponseWithValue(""), nil
		case 2:
			time.Sleep(20 * time.Millisecond)
			return validResponseWithValue(""), nil
		default:
			// straggler way over the cap
			time.Sleep(2 * time.Second)
			return validResponseWithValue("0xslow"), nil
		}
	})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 800*time.Millisecond,
		"maxWaitOnEmpty must bound elapsed time well below the straggler's 2s")
}

// TestWaitCap_NoCap_WaitsForEveryone confirms the default behavior is
// unchanged: zero caps = wait for every participant.
func TestWaitCap_NoCap_WaitsForEveryone(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		if idx == 1 {
			return validResponseWithValue("0x1"), nil
		}
		time.Sleep(250 * time.Millisecond)
		return validResponseWithValue("0x1"), nil
	})
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"with no wait cap, consensus must wait for the slower participant")
}
