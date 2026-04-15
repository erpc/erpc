package consensus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
	_ = telemetry.SetHistogramBuckets("")
}

// TestConsensus_ContextCancelAfterExecution_DoesNotDiscardResults verifies that
// cancelling the parent context after participants have executed does not cause
// ErrConsensusLowParticipants with participants: null.
func TestConsensus_ContextCancelAfterExecution_DoesNotDiscardResults(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := NewConsensusPolicyBuilder().
		WithMaxParticipants(3).
		WithAgreementThreshold(1).
		WithLogger(&logger).
		Build()

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x1"}]}`,
	))

	var started atomic.Int32
	startedCh := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	// Cancel context shortly after participants begin executing
	go func() {
		<-startedCh
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	executor := failsafe.NewExecutor(pol).WithContext(ctx)

	resp, err := executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		if started.Add(1) == 1 {
			close(startedCh)
		}
		// Small sleep so cancel lands while participants are in-flight
		time.Sleep(20 * time.Millisecond)

		jrpc, _ := common.NewJsonRpcResponse(1, []interface{}{}, nil)
		return common.NewNormalizedResponse().WithJsonRpcResponse(jrpc), nil
	})

	// The fix: we should NOT get ErrConsensusLowParticipants here.
	// Before the fix, all participants discarded their valid results on ctx cancel,
	// producing 0 groups -> "participants: null".
	if err != nil {
		assert.False(t,
			common.HasErrorCode(err, common.ErrCodeConsensusLowParticipants),
			"should not get ErrConsensusLowParticipants when participants returned valid results: %v", err,
		)
	} else {
		require.NotNil(t, resp)
	}
}
