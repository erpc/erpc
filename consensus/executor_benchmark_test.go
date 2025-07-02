package consensus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/rs/zerolog"
)

// BenchmarkConsensusExecution benchmarks consensus execution with varying parameters
func BenchmarkConsensusExecution(b *testing.B) {
	scenarios := []struct {
		name                 string
		numUpstreams         int
		requiredParticipants int
		agreementThreshold   int
		consensusRatio       float64
		responseDelay        time.Duration
	}{
		{
			name:                 "Small_5_Upstreams",
			numUpstreams:         5,
			requiredParticipants: 3,
			agreementThreshold:   3,
			consensusRatio:       0.6,
			responseDelay:        1 * time.Millisecond,
		},
		{
			name:                 "Medium_20_Upstreams",
			numUpstreams:         20,
			requiredParticipants: 10,
			agreementThreshold:   10,
			consensusRatio:       0.6,
			responseDelay:        1 * time.Millisecond,
		},
		{
			name:                 "Large_50_Upstreams",
			numUpstreams:         50,
			requiredParticipants: 25,
			agreementThreshold:   25,
			consensusRatio:       0.6,
			responseDelay:        1 * time.Millisecond,
		},
		{
			name:                 "VeryLarge_100_Upstreams",
			numUpstreams:         100,
			requiredParticipants: 50,
			agreementThreshold:   50,
			consensusRatio:       0.6,
			responseDelay:        1 * time.Millisecond,
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			// Setup
			upstreams := make([]common.Upstream, scenario.numUpstreams)
			responses := make([]*common.NormalizedResponse, scenario.numUpstreams)

			consensusCount := int(float64(scenario.numUpstreams) * scenario.consensusRatio)

			for i := 0; i < scenario.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)

				if i < consensusCount {
					responses[i] = createResponse("consensus_result", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.requiredParticipants).
				WithAgreementThreshold(scenario.agreementThreshold).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()

			// Run benchmark
			for n := 0; n < b.N; n++ {
				mockExec := &benchmarkMockExecution{
					responses:     responses,
					upstreams:     upstreams,
					responseDelay: scenario.responseDelay,
				}

				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

					// Simulate work
					time.Sleep(scenario.responseDelay)

					for i, up := range upstreams {
						if up.Id() == upstreamID {
							return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
				})(mockExec)

				if result.Error != nil {
					b.Fatalf("unexpected error: %v", result.Error)
				}
			}
		})
	}
}

// BenchmarkShortCircuit measures the effectiveness of short-circuit optimization
func BenchmarkShortCircuit(b *testing.B) {
	scenarios := []struct {
		name              string
		numUpstreams      int
		consensusPosition string // "early", "middle", "late"
	}{
		{
			name:              "Early_Consensus_20_Upstreams",
			numUpstreams:      20,
			consensusPosition: "early",
		},
		{
			name:              "Middle_Consensus_20_Upstreams",
			numUpstreams:      20,
			consensusPosition: "middle",
		},
		{
			name:              "Late_Consensus_20_Upstreams",
			numUpstreams:      20,
			consensusPosition: "late",
		},
		{
			name:              "Early_Consensus_50_Upstreams",
			numUpstreams:      50,
			consensusPosition: "early",
		},
		{
			name:              "Middle_Consensus_50_Upstreams",
			numUpstreams:      50,
			consensusPosition: "middle",
		},
		{
			name:              "Late_Consensus_50_Upstreams",
			numUpstreams:      50,
			consensusPosition: "late",
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			upstreams := make([]common.Upstream, scenario.numUpstreams)
			responses := make([]*common.NormalizedResponse, scenario.numUpstreams)

			// Configure where consensus appears
			consensusThreshold := scenario.numUpstreams/2 + 1
			var consensusStart int
			switch scenario.consensusPosition {
			case "early":
				consensusStart = 0
			case "middle":
				consensusStart = scenario.numUpstreams/2 - consensusThreshold/2
			case "late":
				consensusStart = scenario.numUpstreams - consensusThreshold
			}

			for i := 0; i < scenario.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)

				if i >= consensusStart && i < consensusStart+consensusThreshold {
					responses[i] = createResponse("consensus", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.numUpstreams).
				WithAgreementThreshold(consensusThreshold).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()

			var totalResponsesCollected int64

			for n := 0; n < b.N; n++ {
				responsesCollected := atomic.Int32{}

				mockExec := &benchmarkMockExecution{
					responses:     responses,
					upstreams:     upstreams,
					responseDelay: 1 * time.Millisecond,
				}

				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

					responsesCollected.Add(1)
					time.Sleep(1 * time.Millisecond)

					for i, up := range upstreams {
						if up.Id() == upstreamID {
							return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
				})(mockExec)

				if result.Error != nil {
					b.Fatalf("unexpected error: %v", result.Error)
				}

				totalResponsesCollected += int64(responsesCollected.Load())
			}

			avgResponsesCollected := float64(totalResponsesCollected) / float64(b.N)
			shortCircuitEfficiency := (1 - avgResponsesCollected/float64(scenario.numUpstreams)) * 100

			b.ReportMetric(avgResponsesCollected, "responses_collected")
			b.ReportMetric(shortCircuitEfficiency, "short_circuit_efficiency_%")
		})
	}
}

// BenchmarkMisbehaviorTracking measures overhead of misbehavior tracking
func BenchmarkMisbehaviorTracking(b *testing.B) {
	scenarios := []struct {
		name                  string
		numUpstreams          int
		misbehavingPercentage float64
		punishMisbehavior     bool
	}{
		{
			name:                  "No_Tracking_10_Upstreams",
			numUpstreams:          10,
			misbehavingPercentage: 0.2,
			punishMisbehavior:     false,
		},
		{
			name:                  "With_Tracking_10_Upstreams",
			numUpstreams:          10,
			misbehavingPercentage: 0.2,
			punishMisbehavior:     true,
		},
		{
			name:                  "No_Tracking_50_Upstreams",
			numUpstreams:          50,
			misbehavingPercentage: 0.2,
			punishMisbehavior:     false,
		},
		{
			name:                  "With_Tracking_50_Upstreams",
			numUpstreams:          50,
			misbehavingPercentage: 0.2,
			punishMisbehavior:     true,
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			upstreams := make([]common.Upstream, scenario.numUpstreams)
			responses := make([]*common.NormalizedResponse, scenario.numUpstreams)

			misbehavingCount := int(float64(scenario.numUpstreams) * scenario.misbehavingPercentage)
			consensusCount := scenario.numUpstreams - misbehavingCount

			for i := 0; i < scenario.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)

				if i < consensusCount {
					responses[i] = createResponse("consensus", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("misbehaving%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			builder := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.numUpstreams).
				WithAgreementThreshold(consensusCount).
				WithLogger(&logger)

			if scenario.punishMisbehavior {
				builder = builder.WithPunishMisbehavior(&common.PunishMisbehaviorConfig{
					DisputeThreshold: 3,
					DisputeWindow:    common.Duration(10 * time.Second),
					SitOutPenalty:    common.Duration(500 * time.Millisecond),
				})
			}

			policy := builder.Build()
			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				mockExec := &benchmarkMockExecution{
					responses:     responses,
					upstreams:     upstreams,
					responseDelay: 100 * time.Microsecond,
				}

				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

					time.Sleep(100 * time.Microsecond)

					for i, up := range upstreams {
						if up.Id() == upstreamID {
							return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
				})(mockExec)

				if result.Error != nil {
					b.Fatalf("unexpected error: %v", result.Error)
				}
			}
		})
	}
}

// BenchmarkConcurrentConsensus measures performance under concurrent load
func BenchmarkConcurrentConsensus(b *testing.B) {
	concurrencyLevels := []int{1, 10, 50, 100}

	for _, concurrency := range concurrencyLevels {
		b.Run(fmt.Sprintf("Concurrency_%d", concurrency), func(b *testing.B) {
			numUpstreams := 20
			upstreams := make([]common.Upstream, numUpstreams)
			responses := make([]*common.NormalizedResponse, numUpstreams)

			for i := 0; i < numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)
				if i < 12 { // 60% consensus
					responses[i] = createResponse("consensus", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(numUpstreams).
				WithAgreementThreshold(11).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					mockExec := &benchmarkMockExecution{
						responses:     responses,
						upstreams:     upstreams,
						responseDelay: 100 * time.Microsecond,
					}

					result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
						ctx := exec.Context()
						req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
						upstreamID := ""
						if req != nil && req.Directives() != nil {
							upstreamID = req.Directives().UseUpstream
						}

						time.Sleep(100 * time.Microsecond)

						for i, up := range upstreams {
							if up.Id() == upstreamID {
								return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
							}
						}
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
					})(mockExec)

					if result.Error != nil {
						b.Fatalf("unexpected error: %v", result.Error)
					}
				}
			})
		})
	}
}

// BenchmarkHashCalculation measures the overhead of hash calculation
func BenchmarkHashCalculation(b *testing.B) {
	responseSizes := []int{100, 1000, 10000, 100000} // bytes

	for _, size := range responseSizes {
		b.Run(fmt.Sprintf("Size_%d_bytes", size), func(b *testing.B) {
			// Create a response with the specified size
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i % 256)
			}

			upstream := common.NewFakeUpstream("test", nil)
			response := createResponse(string(data), upstream)

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: &consensusPolicy[*common.NormalizedResponse]{
					config: &config[*common.NormalizedResponse]{},
				},
			}

			exec := &mockExecution{}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				_, err := executor.resultToHash(response, exec)
				if err != nil {
					b.Fatalf("hash calculation failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkMemoryUsage measures memory allocation patterns
func BenchmarkMemoryUsage(b *testing.B) {
	scenarios := []struct {
		name         string
		numUpstreams int
	}{
		{"10_Upstreams", 10},
		{"50_Upstreams", 50},
		{"100_Upstreams", 100},
		{"200_Upstreams", 200},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			upstreams := make([]common.Upstream, scenario.numUpstreams)
			responses := make([]*common.NormalizedResponse, scenario.numUpstreams)

			for i := 0; i < scenario.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)
				responses[i] = createResponse("consensus", upstreams[i])
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.numUpstreams).
				WithAgreementThreshold(scenario.numUpstreams/2 + 1).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()
			b.ReportAllocs()

			for n := 0; n < b.N; n++ {
				mockExec := &benchmarkMockExecution{
					responses:     responses,
					upstreams:     upstreams,
					responseDelay: 0, // No delay for memory benchmarks
				}

				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

					for i, up := range upstreams {
						if up.Id() == upstreamID {
							return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
				})(mockExec)

				if result.Error != nil {
					b.Fatalf("unexpected error: %v", result.Error)
				}
			}
		})
	}
}

// BenchmarkResponseCollectionStrategies compares different response collection patterns
func BenchmarkResponseCollectionStrategies(b *testing.B) {
	numUpstreams := 50
	upstreams := make([]common.Upstream, numUpstreams)
	responses := make([]*common.NormalizedResponse, numUpstreams)

	for i := 0; i < numUpstreams; i++ {
		upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i), nil)
		if i < 30 { // 60% consensus
			responses[i] = createResponse("consensus", upstreams[i])
		} else {
			responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
		}
	}

	scenarios := []struct {
		name          string
		responseDelay time.Duration
		variableDelay bool
	}{
		{
			name:          "Uniform_Fast_Responses",
			responseDelay: 100 * time.Microsecond,
			variableDelay: false,
		},
		{
			name:          "Uniform_Slow_Responses",
			responseDelay: 5 * time.Millisecond,
			variableDelay: false,
		},
		{
			name:          "Variable_Response_Times",
			responseDelay: 1 * time.Millisecond,
			variableDelay: true,
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(numUpstreams).
				WithAgreementThreshold(26).
				WithLogger(&logger).
				Build()

			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				mockExec := &benchmarkMockExecution{
					responses:     responses,
					upstreams:     upstreams,
					responseDelay: scenario.responseDelay,
					variableDelay: scenario.variableDelay,
				}

				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

					// Find upstream index for variable delay
					upstreamIndex := -1
					for i, up := range upstreams {
						if up.Id() == upstreamID {
							upstreamIndex = i
							break
						}
					}

					if scenario.variableDelay && upstreamIndex >= 0 {
						// Variable delay based on upstream index
						delay := scenario.responseDelay * time.Duration(1+upstreamIndex%5)
						time.Sleep(delay)
					} else {
						time.Sleep(scenario.responseDelay)
					}

					for i, up := range upstreams {
						if up.Id() == upstreamID {
							return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: responses[i]}
						}
					}
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response")}
				})(mockExec)

				if result.Error != nil {
					b.Fatalf("unexpected error: %v", result.Error)
				}
			}
		})
	}
}

// benchmarkMockExecution is a mock execution for benchmarking
type benchmarkMockExecution struct {
	responses     []*common.NormalizedResponse
	upstreams     []common.Upstream
	responseDelay time.Duration
	variableDelay bool
	ctx           context.Context
	mu            sync.Mutex
}

func (m *benchmarkMockExecution) Context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ctx != nil {
		return m.ctx
	}

	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_benchmark"}`))

	m.ctx = context.Background()
	m.ctx = context.WithValue(m.ctx, common.RequestContextKey, dummyReq)
	m.ctx = context.WithValue(m.ctx, common.UpstreamsContextKey, m.upstreams)
	return m.ctx
}

// Implement remaining Execution interface methods
func (m *benchmarkMockExecution) Attempts() int                          { return 1 }
func (m *benchmarkMockExecution) Executions() int                        { return 1 }
func (m *benchmarkMockExecution) Retries() int                           { return 0 }
func (m *benchmarkMockExecution) Hedges() int                            { return 0 }
func (m *benchmarkMockExecution) StartTime() time.Time                   { return time.Now() }
func (m *benchmarkMockExecution) ElapsedTime() time.Duration             { return 0 }
func (m *benchmarkMockExecution) LastResult() *common.NormalizedResponse { return nil }
func (m *benchmarkMockExecution) LastError() error                       { return nil }
func (m *benchmarkMockExecution) IsFirstAttempt() bool                   { return true }
func (m *benchmarkMockExecution) IsRetry() bool                          { return false }
func (m *benchmarkMockExecution) IsHedge() bool                          { return false }
func (m *benchmarkMockExecution) AttemptStartTime() time.Time            { return time.Now() }
func (m *benchmarkMockExecution) ElapsedAttemptTime() time.Duration      { return 0 }
func (m *benchmarkMockExecution) IsCanceled() bool                       { return false }
func (m *benchmarkMockExecution) Canceled() <-chan struct{}              { return nil }
func (m *benchmarkMockExecution) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}
func (m *benchmarkMockExecution) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *benchmarkMockExecution) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.Context(), key, value)
	newExec := &benchmarkMockExecution{
		responses:     m.responses,
		upstreams:     m.upstreams,
		responseDelay: m.responseDelay,
		variableDelay: m.variableDelay,
		ctx:           newCtx,
	}
	return newExec
}
func (m *benchmarkMockExecution) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{}
}
func (m *benchmarkMockExecution) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}
func (m *benchmarkMockExecution) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}
func (m *benchmarkMockExecution) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}
func (m *benchmarkMockExecution) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}
