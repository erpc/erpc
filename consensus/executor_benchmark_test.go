package consensus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))

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
				WithLogger(&logger)

			builtPolicy := policy.Build()
			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			b.ResetTimer()

			// Create execResult objects for testing specific functions
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       context.WithValue(context.WithValue(context.Background(), common.RequestContextKey, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))), common.UpstreamsContextKey, upstreams),
			}

			// Run benchmark - test the core consensus functions directly
			for n := 0; n < b.N; n++ {
				// Test key consensus functions that are used in high RPS scenarios
				logger := zerolog.Nop()

				// 1. Test countResponsesByHash - core consensus evaluation
				_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)

				// 2. Test checkShortCircuit - early termination logic
				_ = executor.checkShortCircuit(&logger, execResults, mockExec)

				// 3. Test handleAcceptMostCommon - dispute resolution
				_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
					return fmt.Errorf("test dispute")
				})
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
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))

				if i >= consensusStart && i < consensusStart+consensusThreshold {
					responses[i] = createResponse("consensus", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			builtPolicy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.numUpstreams).
				WithAgreementThreshold(consensusThreshold).
				WithLogger(&logger).
				Build()

			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for testing checkShortCircuit function directly
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       context.WithValue(context.WithValue(context.Background(), common.RequestContextKey, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))), common.UpstreamsContextKey, upstreams),
			}

			// Calculate consensus timing based on position
			var consensusTiming int
			switch scenario.consensusPosition {
			case "early":
				consensusTiming = scenario.numUpstreams / 4
			case "middle":
				consensusTiming = scenario.numUpstreams / 2
			case "late":
				consensusTiming = scenario.numUpstreams * 3 / 4
			default:
				consensusTiming = scenario.numUpstreams / 2
			}
			if consensusTiming == 0 {
				consensusTiming = 1
			}

			b.ResetTimer()

			// Test checkShortCircuit function directly with real objects
			for n := 0; n < b.N; n++ {
				logger := zerolog.Nop()

				// Test the actual short-circuit logic based on consensus timing scenario
				partialResults := execResults[:consensusTiming]

				// This tests the core short-circuit optimization
				shortCircuited := executor.checkShortCircuit(&logger, partialResults, mockExec)

				// Also test related consensus functions
				_, _, _ = executor.countResponsesByHash(&logger, partialResults, mockExec)

				_ = shortCircuited // Use the result to prevent optimization
			}

			// Report that we're testing short-circuit efficiency with real function calls
			consensusTimingPercent := float64(scenario.numUpstreams/2) / float64(scenario.numUpstreams) * 100
			b.ReportMetric(consensusTimingPercent, "consensus_timing_%")
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
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))

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

			builtPolicy := builder.Build()
			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for testing misbehavior tracking functions directly
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       context.WithValue(context.WithValue(context.Background(), common.RequestContextKey, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))), common.UpstreamsContextKey, upstreams),
			}

			b.ResetTimer()

			// Test misbehavior tracking functions directly with real objects
			for n := 0; n < b.N; n++ {
				logger := zerolog.Nop()

				// Test the core consensus functions that handle misbehavior tracking
				_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)

				// Test dispute resolution logic which triggers misbehavior tracking
				_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
					return fmt.Errorf("test dispute for misbehavior")
				})

				// Test short-circuit logic which also tracks responses
				_ = executor.checkShortCircuit(&logger, execResults, mockExec)
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
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
				if i < 12 { // 60% consensus
					responses[i] = createResponse("consensus", upstreams[i])
				} else {
					responses[i] = createResponse(fmt.Sprintf("result%d", i), upstreams[i])
				}
			}

			logger := zerolog.Nop()
			builtPolicy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(numUpstreams).
				WithAgreementThreshold(11).
				WithLogger(&logger).
				Build()

			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for concurrent testing
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       context.WithValue(context.WithValue(context.Background(), common.RequestContextKey, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))), common.UpstreamsContextKey, upstreams),
			}

			b.ResetTimer()

			// Test concurrent access to consensus functions with real objects
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					logger := zerolog.Nop()

					// Test concurrent execution of core consensus functions
					_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)
					_ = executor.checkShortCircuit(&logger, execResults, mockExec)
					_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
						return fmt.Errorf("concurrent test dispute")
					})
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

			upstream := common.NewFakeUpstream("test")
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
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
				responses[i] = createResponse("consensus", upstreams[i])
			}

			logger := zerolog.Nop()
			builtPolicy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.numUpstreams).
				WithAgreementThreshold(scenario.numUpstreams/2 + 1).
				WithLogger(&logger).
				Build()

			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for memory testing
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       context.WithValue(context.WithValue(context.Background(), common.RequestContextKey, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))), common.UpstreamsContextKey, upstreams),
			}

			b.ResetTimer()
			b.ReportAllocs()

			// Test memory usage of consensus functions directly
			for n := 0; n < b.N; n++ {
				logger := zerolog.Nop()

				// Test memory allocations in core consensus functions
				_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)
				_ = executor.checkShortCircuit(&logger, execResults, mockExec)
				_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
					return fmt.Errorf("memory test dispute")
				})
			}
		})
	}
}

// BenchmarkHotPathPerformance specifically targets the map allocation hot paths
func BenchmarkHotPathPerformance(b *testing.B) {
	scenarios := []struct {
		name                 string
		numUpstreams         int
		requiredParticipants int
		agreementThreshold   int
		consensusRatio       float64
	}{
		{
			name:                 "Small_Load_10_Upstreams",
			numUpstreams:         10,
			requiredParticipants: 10,
			agreementThreshold:   6,
			consensusRatio:       0.6,
		},
		{
			name:                 "Medium_Load_50_Upstreams",
			numUpstreams:         50,
			requiredParticipants: 50,
			agreementThreshold:   30,
			consensusRatio:       0.6,
		},
		{
			name:                 "Heavy_Load_100_Upstreams",
			numUpstreams:         100,
			requiredParticipants: 100,
			agreementThreshold:   60,
			consensusRatio:       0.6,
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			// Setup realistic responses with mix of empty and non-empty
			upstreams := make([]common.Upstream, scenario.numUpstreams)
			responses := make([]*common.NormalizedResponse, scenario.numUpstreams)

			consensusCount := int(float64(scenario.numUpstreams) * scenario.consensusRatio)
			emptyCount := scenario.numUpstreams / 4 // 25% empty responses

			for i := 0; i < scenario.numUpstreams; i++ {
				upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))

				if i < consensusCount {
					responses[i] = createResponse("0x12345", upstreams[i]) // Non-empty consensus
				} else if i < consensusCount+emptyCount {
					responses[i] = createResponse("[]", upstreams[i]) // Empty response
				} else {
					responses[i] = createResponse(fmt.Sprintf("0xresult%d", i), upstreams[i]) // Different non-empty
				}
			}

			logger := zerolog.Nop()
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(scenario.requiredParticipants).
				WithAgreementThreshold(scenario.agreementThreshold).
				WithLogger(&logger)

			builtPolicy := policy.Build()
			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for hot path testing
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			// Create request context
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"]}`))
			ctx := context.WithValue(context.Background(), common.RequestContextKey, req)
			ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

			mockExec := &benchmarkMockExecution{
				responses: responses,
				upstreams: upstreams,
				ctx:       ctx,
			}

			// Use real hot path functions to exercise the actual performance bottlenecks
			b.ResetTimer()
			b.ReportAllocs()

			for n := 0; n < b.N; n++ {
				logger := zerolog.Nop()

				// Test the actual hot path functions that are used in high RPS scenarios
				_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)
				_ = executor.checkShortCircuit(&logger, execResults, mockExec)
				_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
					return fmt.Errorf("hot path test dispute")
				})
			}
		})
	}
}

// BenchmarkMapAllocations specifically measures map allocation overhead in hot paths
func BenchmarkMapAllocations(b *testing.B) {
	numUpstreams := 50
	upstreams := make([]common.Upstream, numUpstreams)
	responses := make([]*execResult[*common.NormalizedResponse], numUpstreams)

	// Create realistic mix of responses
	for i := 0; i < numUpstreams; i++ {
		upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
		if i < 30 {
			resp := createResponse("0x12345", upstreams[i]) // Consensus result
			responses[i] = &execResult[*common.NormalizedResponse]{
				result:   resp,
				err:      nil,
				index:    i,
				upstream: upstreams[i],
			}
		} else if i < 40 {
			resp := createResponse("[]", upstreams[i]) // Empty result
			responses[i] = &execResult[*common.NormalizedResponse]{
				result:   resp,
				err:      nil,
				index:    i,
				upstream: upstreams[i],
			}
		} else {
			resp := createResponse(fmt.Sprintf("0xresult%d", i), upstreams[i]) // Different result
			responses[i] = &execResult[*common.NormalizedResponse]{
				result:   resp,
				err:      nil,
				index:    i,
				upstream: upstreams[i],
			}
		}
	}

	logger := zerolog.Nop()
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithRequiredParticipants(numUpstreams).
		WithAgreementThreshold(25).
		WithLogger(&logger)

	builtPolicy := policy.Build().(*consensusPolicy[*common.NormalizedResponse])
	executor := builtPolicy.Build().(*executor[*common.NormalizedResponse])

	mockExec := &benchmarkMockExecution{
		responses: make([]*common.NormalizedResponse, numUpstreams),
		upstreams: upstreams,
		ctx:       context.Background(),
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		// Test the specific hot path functions that allocate maps

		// 1. Test countResponsesByHash (allocates multiple maps)
		_, _, _ = executor.countResponsesByHash(&logger, responses, mockExec)

		// 2. Test checkShortCircuit (allocates resultCounts and emptyishHashes maps)
		_ = executor.checkShortCircuit(&logger, responses, mockExec)

		// 3. Test handleAcceptMostCommon (allocates nonEmptyResults, emptyResults, resultsByHash)
		_ = executor.handleAcceptMostCommon(context.Background(), &logger, responses, mockExec, func() error {
			return fmt.Errorf("test error")
		})
	}
}

// BenchmarkResponseCollectionStrategies compares different response collection patterns
func BenchmarkResponseCollectionStrategies(b *testing.B) {
	numUpstreams := 50
	upstreams := make([]common.Upstream, numUpstreams)
	responses := make([]*common.NormalizedResponse, numUpstreams)

	for i := 0; i < numUpstreams; i++ {
		upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i))
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
			builtPolicy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(30).
				WithAgreementThreshold(26).
				WithLogger(&logger).
				Build()

			executor := builtPolicy.(*consensusPolicy[*common.NormalizedResponse]).Build().(*executor[*common.NormalizedResponse])

			// Create execResult objects for testing response collection
			execResults := make([]*execResult[*common.NormalizedResponse], len(responses))
			for i, resp := range responses {
				execResults[i] = &execResult[*common.NormalizedResponse]{
					result:   resp,
					err:      nil,
					index:    i,
					upstream: upstreams[i],
				}
			}

			mockExec := &benchmarkMockExecution{
				responses:     responses,
				upstreams:     upstreams,
				responseDelay: scenario.responseDelay,
				variableDelay: scenario.variableDelay,
			}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				// Test response collection and consensus functions directly
				logger := zerolog.Nop()

				// 1. Test countResponsesByHash - response collection and consensus evaluation
				_, _, _ = executor.countResponsesByHash(&logger, execResults, mockExec)

				// 2. Test checkShortCircuit - early termination based on response patterns
				_ = executor.checkShortCircuit(&logger, execResults, mockExec)

				// 3. Test handleAcceptMostCommon - response validation and acceptance
				_ = executor.handleAcceptMostCommon(context.Background(), &logger, execResults, mockExec, func() error {
					return fmt.Errorf("test collection strategy")
				})
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

// BenchmarkStringAllocations measures performance of string operations in hot paths
func BenchmarkStringAllocations(b *testing.B) {
	scenarios := []struct {
		name      string
		operation string
		testCount int
	}{
		{"ErrorHash_Small", "error_hash", 100},
		{"ErrorHash_Large", "error_hash", 1000},
		{"LoggingFields_Small", "logging", 10},
		{"LoggingFields_Large", "logging", 50},
		{"RequestID_Formatting", "request_id", 100},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()

			switch scenario.operation {
			case "error_hash":
				// Test error hash generation (fmt.Sprintf hotspot)
				for n := 0; n < b.N; n++ {
					for i := 0; i < scenario.testCount; i++ {
						// This mirrors the hot path in errorToConsensusHash
						hash := fmt.Sprintf("jsonrpc:%d", i*3+1000) // Simulate normalized error codes
						_ = hash
					}
				}

			case "logging":
				// Test logging field generation (fmt.Sprintf hotspots)
				for n := 0; n < b.N; n++ {
					for i := 0; i < scenario.testCount; i++ {
						// This mirrors hot paths in logResponseDetails
						upstream := fmt.Sprintf("upstream%d", i)
						errorField := fmt.Sprintf("error%d", i)
						hashField := fmt.Sprintf("hash%d", i)
						responseField := fmt.Sprintf("response%d", i)
						_ = upstream + errorField + hashField + responseField
					}
				}

			case "request_id":
				// Test request ID formatting (fmt.Sprintf hotspot)
				for n := 0; n < b.N; n++ {
					for i := 0; i < scenario.testCount; i++ {
						// This mirrors hot path in consensus span attributes
						requestId := fmt.Sprintf("%v", i*1000+123)
						_ = requestId
					}
				}
			}
		})
	}
}

// BenchmarkJSONMarshaling measures performance of JSON operations in hash calculations
func BenchmarkJSONMarshaling(b *testing.B) {
	scenarios := []struct {
		name       string
		dataSize   string
		resultType string
	}{
		{"Small_Number", "small", "number"},
		{"Small_String", "small", "string"},
		{"Small_Array", "small", "array"},
		{"Small_Object", "small", "object"},
		{"Medium_Array", "medium", "array"},
		{"Medium_Object", "medium", "object"},
		{"Large_Array", "large", "array"},
		{"Large_Object", "large", "object"},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.name, func(b *testing.B) {
			// Create test JSON data of different types and sizes
			var testResult []byte
			switch scenario.resultType {
			case "number":
				testResult = []byte("12345")
			case "string":
				if scenario.dataSize == "small" {
					testResult = []byte(`"0x1234567890abcdef"`)
				} else {
					testResult = []byte(`"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"`)
				}
			case "array":
				switch scenario.dataSize {
				case "small":
					testResult = []byte(`["0x123", "0x456", "0x789"]`)
				case "medium":
					testResult = []byte(`["0x123", "0x456", "0x789", "0xabc", "0xdef", "0x111", "0x222", "0x333", "0x444", "0x555"]`)
				case "large":
					elements := make([]string, 100)
					for i := 0; i < 100; i++ {
						elements[i] = fmt.Sprintf(`"0x%x"`, i*1000)
					}
					testResult = []byte(`[` + strings.Join(elements, ",") + `]`)
				}
			case "object":
				switch scenario.dataSize {
				case "small":
					testResult = []byte(`{"blockNumber":"0x123","gasUsed":"0x456","timestamp":"0x789"}`)
				case "medium":
					testResult = []byte(`{"blockNumber":"0x123","gasUsed":"0x456","timestamp":"0x789","hash":"0xabc","parentHash":"0xdef","difficulty":"0x111","totalDifficulty":"0x222","size":"0x333","gasLimit":"0x444"}`)
				case "large":
					fields := make([]string, 50)
					for i := 0; i < 50; i++ {
						fields[i] = fmt.Sprintf(`"field%d":"0x%x"`, i, i*1000)
					}
					testResult = []byte(`{` + strings.Join(fields, ",") + `}`)
				}
			}

			// Create a JsonRpcResponse to test hash calculation
			jrr, err := common.NewJsonRpcResponse(1, testResult, nil)
			if err != nil {
				b.Fatalf("Failed to create test response: %v", err)
			}

			b.ResetTimer()
			b.ReportAllocs()

			// Test the actual hash calculation that's used in hot paths
			for n := 0; n < b.N; n++ {
				// This is the actual hot path: CanonicalHash() which does JSON unmarshaling + canonicalization + SHA256
				hash, err := jrr.CanonicalHash()
				if err != nil {
					b.Fatalf("Hash calculation failed: %v", err)
				}
				_ = hash
			}
		})
	}
}

// BenchmarkHashCalculationComponents breaks down hash calculation into components
func BenchmarkHashCalculationComponents(b *testing.B) {
	testData := []byte(`{"blockNumber":"0x123","gasUsed":"0x456","timestamp":"0x789","hash":"0xabc","parentHash":"0xdef"}`)
	jrr, _ := common.NewJsonRpcResponse(1, testData, nil)

	b.Run("JSONUnmarshal", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			var obj interface{}
			err := json.Unmarshal(testData, &obj)
			if err != nil {
				b.Fatal(err)
			}
			_ = obj
		}
	})

	b.Run("Canonicalization", func(b *testing.B) {
		var obj interface{}
		json.Unmarshal(testData, &obj)
		b.ResetTimer()
		b.ReportAllocs()

		for n := 0; n < b.N; n++ {
			// This calls the internal canonicalize function
			_, err := jrr.CanonicalHash()
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("FullHashCalculation", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			hash, err := jrr.CanonicalHash()
			if err != nil {
				b.Fatal(err)
			}
			_ = hash
		}
	})
}
