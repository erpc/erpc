package erpc

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
)

const TEST_DEFAULT_BATCH_SIZE = 500

func init() {
	util.ConfigureTestLogger()
}

// MockUpstreamServer simulates an upstream node with configurable behavior
type MockUpstreamServer struct {
	ID             string
	ResponseDelay  time.Duration
	ResponseJitter time.Duration
	ErrorRate      float64
	server         *httptest.Server
	requestCount   atomic.Int64
	errorCount     atomic.Int64
	mu             sync.Mutex
	spikeConfig    *SpikeConfig
}

// SpikeConfig defines spike behavior for testing
type SpikeConfig struct {
	Enabled       bool
	SpikeDelay    time.Duration
	SpikeInterval int // Every N requests
}

// NewMockUpstreamServer creates a new mock upstream server
func NewMockUpstreamServer(id string, respDelay time.Duration, respJitter time.Duration, errorRate float64) *MockUpstreamServer {
	m := &MockUpstreamServer{
		ID:             id,
		ResponseDelay:  respDelay,
		ResponseJitter: respJitter,
		ErrorRate:      errorRate,
	}

	// Create the HTTP handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.handleRequest(w, r)
	})

	// Start the test server
	m.server = httptest.NewServer(handler)

	return m
}

// handleRequest processes incoming requests with configured behavior
func (m *MockUpstreamServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Track request count
	reqNum := m.requestCount.Add(1)

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Parse JSON-RPC request
	var jsonReq map[string]interface{}
	if err := json.Unmarshal(body, &jsonReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract request ID and method
	reqID := jsonReq["id"]
	method := jsonReq["method"]

	// Apply configured delay (with optional spike)
	delay := m.ResponseDelay
	if m.spikeConfig != nil && m.spikeConfig.Enabled {
		if int(reqNum)%m.spikeConfig.SpikeInterval == 0 {
			delay = m.spikeConfig.SpikeDelay
		}
	}
	time.Sleep(delay + time.Duration(rand.Float64()*float64(m.ResponseJitter)))

	// Decide if this request should error based on error rate
	shouldError := false
	if m.ErrorRate > 0 {
		shouldError = rand.Float64() < m.ErrorRate
	}

	w.Header().Set("Content-Type", "application/json")

	if shouldError {
		m.errorCount.Add(1)
		// Return a JSON-RPC error
		errorResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]interface{}{
				"code":    -32000,
				"message": "Mock upstream error",
			},
		}
		json.NewEncoder(w).Encode(errorResp)
		return
	}

	// Return a successful response based on method
	var result interface{}
	switch method {
	case "eth_chainId":
		result = "0x7b"
	case "eth_blockNumber":
		result = "0x1234567"
	case "eth_getBalance":
		result = "0x1000000000000000000"
	case "eth_getTransactionCount":
		result = "0x10"
	case "eth_getLogs":
		result = []interface{}{}
	case "eth_getTransactionReceipt":
		result = map[string]interface{}{
			"blockNumber": "0x1234567",
			"status":      "0x1",
		}
	default:
		// Generic successful response
		result = "0x1"
	}

	successResp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  result,
	}
	json.NewEncoder(w).Encode(successResp)
}

// URL returns the server URL
func (m *MockUpstreamServer) URL() string {
	return m.server.URL
}

// Close shuts down the server
func (m *MockUpstreamServer) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

// Stats returns request and error counts
func (m *MockUpstreamServer) Stats() (requests int64, errors int64) {
	return m.requestCount.Load(), m.errorCount.Load()
}

// TestUpstreamDegradationScenarios tests various upstream behavior scenarios
func TestUpstreamDegradationScenarios(t *testing.T) {
	scenarios := []TestScenario{
		{
			Name:                    "TwoBadOneGood",
			Description:             "Two slow upstreams (6s, 3s) and one fast upstream (200ms) - should heavily favor the fast one",
			NumRequests:             TEST_DEFAULT_BATCH_SIZE * 6,
			ExpectedP50:             300*time.Millisecond + 20*time.Millisecond, /*overhead*/
			ExpectedP90:             300*time.Millisecond + 50*time.Millisecond, /*overhead*/
			MaxErrorRate:            0.05,
			ExpectedFinalScoreOrder: []string{"fast_upstream", "slow_upstream", "worst_upstream"},
			NetworkFailsafe: &common.FailsafeConfig{
				Timeout: &common.TimeoutPolicyConfig{
					Duration: common.Duration(30 * time.Second),
				},
				Hedge: &common.HedgePolicyConfig{
					MaxCount: 2,
					Quantile: 0.95,
					MinDelay: common.Duration(100 * time.Millisecond),
					MaxDelay: common.Duration(5 * time.Second),
				},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       common.Duration(100 * time.Millisecond),
				},
			},
			Upstreams: []UpstreamBehavior{
				{
					ID:            "fast_upstream",
					ResponseDelay: 300 * time.Millisecond,
					ErrorRate:     0,
					MinPercent:    95, // most traffic
					ScoreMultipliers: []*common.ScoreMultiplierConfig{
						{
							Overall:         util.Float64Ptr(1.0),
							RespLatency:     util.Float64Ptr(10.0),
							ErrorRate:       util.Float64Ptr(1.0),
							TotalRequests:   util.Float64Ptr(0.0),
							ThrottledRate:   util.Float64Ptr(2.0),
							BlockHeadLag:    util.Float64Ptr(2.0),
							FinalizationLag: util.Float64Ptr(1.0),
						},
					},
				},
				{
					ID:            "worst_upstream",
					ResponseDelay: 6 * time.Second,
					ErrorRate:     0,
					MaxPercent:    5, // very little traffic initially
					ScoreMultipliers: []*common.ScoreMultiplierConfig{
						{
							Overall:         util.Float64Ptr(1.0),
							RespLatency:     util.Float64Ptr(10.0),
							ErrorRate:       util.Float64Ptr(1.0),
							TotalRequests:   util.Float64Ptr(0.0),
							ThrottledRate:   util.Float64Ptr(2.0),
							BlockHeadLag:    util.Float64Ptr(2.0),
							FinalizationLag: util.Float64Ptr(1.0),
						},
					},
				},
				{
					ID:            "slow_upstream",
					ResponseDelay: 2 * time.Second,
					ErrorRate:     0,
					MaxPercent:    5, // very little traffic initially
					ScoreMultipliers: []*common.ScoreMultiplierConfig{
						{
							Overall:         util.Float64Ptr(1.0),
							RespLatency:     util.Float64Ptr(10.0),
							ErrorRate:       util.Float64Ptr(1.0),
							TotalRequests:   util.Float64Ptr(0.0),
							ThrottledRate:   util.Float64Ptr(2.0),
							BlockHeadLag:    util.Float64Ptr(2.0),
							FinalizationLag: util.Float64Ptr(1.0),
						},
					},
				},
			},
		},
	}

	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			scenario.NumRequests = scaledNumRequests(t, scenario.NumRequests)
			runUpstreamTest(t, scenario)
		})
	}
}

// UpstreamBehavior defines how an upstream should behave in tests
type UpstreamBehavior struct {
	ID             string
	Endpoint       string
	ResponseDelay  time.Duration
	ResponseJitter time.Duration
	ErrorRate      float64 // 0.0 to 1.0 - probability of returning an error
	// For more complex behaviors
	DelayVariance    time.Duration // Random variance in delay
	OccasionalSpikes bool          // Sometimes has much longer delays
	SpikeDelay       time.Duration
	SpikeRate        float64 // Probability of a spike

	// Failsafe configuration for this upstream
	FailsafeConfig *common.FailsafeConfig // If nil, uses default

	// Routing score multipliers for this upstream (passed through to config)
	ScoreMultipliers []*common.ScoreMultiplierConfig

	// Expectations (percent of successful traffic routed to this upstream)
	MinPercent float64 // 0-100; 0 means no minimum expectation
	MaxPercent float64 // 0-100; 0 means no maximum expectation
}

// TestScenario defines a complete test scenario
type TestScenario struct {
	Name         string
	Upstreams    []UpstreamBehavior
	NumRequests  int
	ExpectedP50  time.Duration
	ExpectedP90  time.Duration
	MaxErrorRate float64
	Description  string

	// Network-level failsafe configuration
	NetworkFailsafe *common.FailsafeConfig // If nil, uses default

	// Scoring configuration
	ScoreMetricsWindow time.Duration // If 0, uses default

	// Expected final score order (by upstream ID) for network "evm:123" and method "*"
	ExpectedFinalScoreOrder []string
}

// TestResult captures the results of a test run
type TestResult struct {
	Duration     time.Duration
	StatusCode   int
	Body         string
	Error        bool
	UpstreamUsed string
	RequestID    int
	ActualDelay  time.Duration
}

// getDefaultUpstreamFailsafe returns default upstream failsafe config
func getDefaultUpstreamFailsafe(timeout time.Duration) *common.FailsafeConfig {
	return &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: common.Duration(timeout),
		},
	}
}

// createUpstreamConfig creates upstream configuration from behavior
func createUpstreamConfig(behavior UpstreamBehavior) *common.UpstreamConfig {
	// Use provided failsafe or create default
	failsafeConfig := behavior.FailsafeConfig
	if failsafeConfig == nil {
		// Default timeout based on response delay
		timeout := behavior.ResponseDelay * 3
		if timeout < 2*time.Second {
			timeout = 2 * time.Second
		}
		failsafeConfig = getDefaultUpstreamFailsafe(timeout)
	}

	return &common.UpstreamConfig{
		Id:       behavior.ID,
		Type:     common.UpstreamTypeEvm,
		Endpoint: behavior.Endpoint,
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
		JsonRpc: &common.JsonRpcUpstreamConfig{
			SupportsBatch: &common.FALSE,
		},
		Failsafe: []*common.FailsafeConfig{failsafeConfig},
		Routing: &common.RoutingConfig{
			ScoreMultipliers: behavior.ScoreMultipliers,
		},
	}
}

// runUpstreamTest runs a test scenario and returns results
func runUpstreamTest(t *testing.T, scenario TestScenario) {
	// Step 1: Create and start mock upstream servers FIRST
	mockServers := make([]*MockUpstreamServer, 0, len(scenario.Upstreams))
	defer func() {
		// Clean up servers at the end
		for _, server := range mockServers {
			server.Close()
		}
	}()

	// Create all mock servers and get their URLs
	for i, behavior := range scenario.Upstreams {
		// Create mock server with configured behavior
		mockServer := NewMockUpstreamServer(behavior.ID, behavior.ResponseDelay, behavior.ResponseJitter, behavior.ErrorRate)

		// Configure spikes if needed
		if behavior.OccasionalSpikes {
			mockServer.spikeConfig = &SpikeConfig{
				Enabled:       true,
				SpikeDelay:    behavior.SpikeDelay,
				SpikeInterval: int(1.0 / behavior.SpikeRate), // Convert rate to interval
			}
		}

		mockServers = append(mockServers, mockServer)

		// Update behavior endpoint to use real server URL
		scenario.Upstreams[i].Endpoint = mockServer.URL()
		t.Logf("[mock-server] Created mock server for %s at %s", behavior.ID, mockServer.URL())
	}

	// Step 2: Set up gock for background state poller only (after servers are running)
	// util.ResetGock() already enables networking for localhost connections
	util.ResetGock()
	defer util.ResetGock()

	// Step 3: Create upstream configs with real server URLs
	upstreamConfigs := make([]*common.UpstreamConfig, len(scenario.Upstreams))
	for i, behavior := range scenario.Upstreams {
		upstreamConfigs[i] = createUpstreamConfig(behavior)
	}

	// Use provided network failsafe
	networkFailsafe := scenario.NetworkFailsafe

	// Use provided scoring window or default
	scoreWindow := scenario.ScoreMetricsWindow
	if scoreWindow == 0 {
		scoreWindow = 1 * time.Second
	}

	// Scenario preface logs
	t.Logf("[setup] Starting scenario %q: %d upstream(s), %d request(s)", scenario.Name, len(scenario.Upstreams), scenario.NumRequests)
	for _, u := range scenario.Upstreams {
		spikeInfo := ""
		if u.OccasionalSpikes {
			spikeInfo = fmt.Sprintf(", spikes: rate=%.0f%% delay=%s", u.SpikeRate*100, u.SpikeDelay)
		}
		varInfo := ""
		if u.DelayVariance > 0 {
			varInfo = fmt.Sprintf(", variance=±%s", u.DelayVariance/2)
		}
		// The endpoint now contains the actual server URL
		t.Logf("[setup] Upstream %q → %s (delay=%s%s, errorRate=%.0f%%%s)", u.ID, u.Endpoint, u.ResponseDelay, varInfo, u.ErrorRate*100, spikeInfo)
	}
	// Safely format timeout duration
	var nfTimeout time.Duration
	if networkFailsafe != nil && networkFailsafe.Timeout != nil {
		nfTimeout = time.Duration(networkFailsafe.Timeout.Duration)
	}
	t.Logf("[setup] Network failsafe: timeout=%s, hedge=%v, retry=%v; scoringWindow=%s",
		nfTimeout, networkFailsafe.Hedge, networkFailsafe.Retry, scoreWindow)

	// Configuration
	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(180 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id:                     "test_project",
				ScoreMetricsWindowSize: common.Duration(scoreWindow),
				ScoreRefreshInterval:   common.Duration(100 * time.Millisecond),
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
						Failsafe: []*common.FailsafeConfig{networkFailsafe},
					},
				},
				Upstreams: upstreamConfigs,
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	// Set up state poller mocks
	util.SetupMocksForEvmStatePoller()
	t.Logf("[setup] State poller mocks initialized")

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	defer shutdown()

	// Wait for server to be ready
	// time.Sleep(2 * time.Second)

	// Score logging helpers
	logCurrentScoreOrder := func(tag string) ([]string, string) {
		if erpcInstance == nil {
			return nil, ""
		}
		prj, err := erpcInstance.GetProject("test_project")
		if err != nil || prj == nil {
			return nil, ""
		}
		h, err := prj.GatherHealthInfo()
		if err != nil {
			return nil, ""
		}
		networkId := "evm:123"
		order := h.SortedUpstreams[networkId]["*"]
		selectedMethod := "*"
		if len(order) == 0 {
			// try to fall back to any available method bucket
			if methods, ok := h.SortedUpstreams[networkId]; ok {
				for m, lst := range methods {
					if len(lst) > 0 {
						order = lst
						selectedMethod = m
						break
					}
				}
			}
		}
		t.Logf("[scores] order[%s]=%v", tag, order)
		// Also log numeric scores alongside the order
		if len(order) > 0 {
			pairs := make([]string, 0, len(order))
			for _, id := range order {
				var score float64
				if nets, ok := h.UpstreamScores[id]; ok {
					if meths, ok := nets[networkId]; ok {
						if s, ok := meths[selectedMethod]; ok {
							score = s
						} else if s, ok := meths["*"]; ok {
							score = s
						}
					} else if meths, ok := nets["*"]; ok {
						if s, ok := meths[selectedMethod]; ok {
							score = s
						} else if s, ok := meths["*"]; ok {
							score = s
						}
					}
				}
				pairs = append(pairs, fmt.Sprintf("%s:%.3f", id, score))
			}
			t.Logf("[scores] details[%s](method=%s)= [%s]", tag, selectedMethod, strings.Join(pairs, ", "))
		}
		return order, selectedMethod
	}

	// Initial score order snapshot
	logCurrentScoreOrder("initial")

	// Schedule a mid-run snapshot after 2s
	go func() {
		time.Sleep(2 * time.Second)
		logCurrentScoreOrder("t+2s")
	}()

	// Helper to build request body for a given index
	results := make([]TestResult, scenario.NumRequests)
	var completed int32

	buildRequestBody := func(idx int) string {
		switch idx % 3 {
		case 0:
			return fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"latest","toBlock":"latest","address":"0x%040x"}],"id":%d}`, idx, idx+1)
		case 1:
			return fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x%040x","latest"],"id":%d}`, idx, idx+1)
		default:
			return fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getTransactionCount","params":["0x%040x","latest"],"id":%d}`, idx, idx+1)
		}
	}

	// Helper: run one batch (prepare then send)
	runBatchRequests := func(batchStart, batchEnd int) {
		// Phase 1: prepare bodies
		preparedBodies := make([]string, batchEnd-batchStart)
		for i := batchStart; i < batchEnd; i++ {
			preparedBodies[i-batchStart] = buildRequestBody(i)
			if i%10 == 0 {
				method := []string{"eth_getLogs", "eth_getBalance", "eth_getTransactionCount"}[i%3]
				t.Logf("[prep] Prepared request %d/%d (%s)", i+1, scenario.NumRequests, method)
			}
		}

		// Phase 2: send
		var batchWG sync.WaitGroup
		for i := batchStart; i < batchEnd; i++ {
			idx := i
			batchWG.Add(1)
			time.Sleep(10 * time.Millisecond)
			go func() {
				defer batchWG.Done()
				reqBody := preparedBodies[idx-batchStart]
				reqStart := time.Now()
				if idx%10 == 0 {
					method := []string{"eth_getLogs", "eth_getBalance", "eth_getTransactionCount"}[idx%3]
					t.Logf("[send] Sending request %d/%d (%s)", idx+1, scenario.NumRequests, method)
				}
				statusCode, headers, body := sendRequest(reqBody, nil, nil)
				duration := time.Since(reqStart)

				result := TestResult{
					Duration:    duration,
					StatusCode:  statusCode,
					Body:        body,
					RequestID:   idx,
					ActualDelay: duration,
				}

				if headers != nil && headers["X-Erpc-Upstream"] != "" {
					result.UpstreamUsed = headers["X-Erpc-Upstream"]
				}
				if headers != nil && headers["X-Erpc-Duration"] != "" {
					durationMs, err := strconv.ParseInt(headers["X-Erpc-Duration"], 10, 64)
					if err == nil {
						result.Duration = time.Duration(durationMs) * time.Millisecond
					}
				}

				if statusCode == http.StatusOK && !strings.Contains(body, `"error":`) {
					var resp map[string]interface{}
					if err := json.Unmarshal([]byte(body), &resp); err == nil {
						if upstream, ok := resp["_upstream"].(string); ok {
							result.UpstreamUsed = upstream
						}
					}
				} else {
					result.Error = true
					t.Logf("[error] Request %d failed (status=%d, upstream=%q) body=%s", idx+1, statusCode, result.UpstreamUsed, truncate(body, 200))
				}

				results[idx] = result
				c := atomic.AddInt32(&completed, 1)
				if c%10 == 0 {
					t.Logf("[done] Completed %d/%d requests", c, scenario.NumRequests)
				}
			}()
		}
		batchWG.Wait()
	}

	// Batched execution to reduce resource usage
	start := time.Now()
	batchSize := minInt(TEST_DEFAULT_BATCH_SIZE, scenario.NumRequests)
	for batchStart := 0; batchStart < scenario.NumRequests; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > scenario.NumRequests {
			batchEnd = scenario.NumRequests
		}
		runBatchRequests(batchStart, batchEnd)
		// Small pause between batches to ease resource pressure
		time.Sleep(10 * time.Millisecond)
		// Extra score logs to ensure visibility
		batchIdx := (batchStart / batchSize) + 1
		if batchIdx == 1 || batchIdx%5 == 0 {
			logCurrentScoreOrder(fmt.Sprintf("after-batch-%d", batchIdx))
		}
	}
	totalTime := time.Since(start)

	// Final score order snapshot
	finalOrder, _ := logCurrentScoreOrder("final")

	// Analyze results
	analyzeResults(t, scenario, results, totalTime, finalOrder, mockServers)
}

// analyzeResults analyzes test results and validates expectations
func analyzeResults(t *testing.T, scenario TestScenario, results []TestResult, totalTime time.Duration, finalOrder []string, mockServers []*MockUpstreamServer) {
	// Count upstream usage
	upstreamCounts := make(map[string]int)
	for _, behavior := range scenario.Upstreams {
		upstreamCounts[behavior.ID] = 0
	}
	upstreamCounts["unknown"] = 0

	var errorCount int
	var validDurations []time.Duration

	for _, result := range results {
		if result.Error {
			errorCount++
		} else {
			validDurations = append(validDurations, result.Duration)
			if result.UpstreamUsed != "" {
				upstreamCounts[result.UpstreamUsed]++
			} else {
				upstreamCounts["unknown"]++
			}
		}
	}

	// Calculate percentiles
	sort.Slice(validDurations, func(i, j int) bool {
		return validDurations[i] < validDurations[j]
	})

	var p50, p90, p99 time.Duration
	if len(validDurations) > 0 {
		p50 = validDurations[len(validDurations)/2]
		if len(validDurations) > 9 {
			p90 = validDurations[len(validDurations)*9/10]
		}
		p99 = validDurations[len(validDurations)-1]
	}

	errorRate := float64(errorCount) / float64(len(results))

	totalSuccessful := len(results) - errorCount

	// Log results
	t.Logf("=== Test Scenario: %s ===", scenario.Name)
	t.Logf("Description: %s", scenario.Description)
	t.Logf("Total time: %v for %d requests", totalTime, len(results))
	t.Logf("Error rate: %.1f%% (%d/%d)", errorRate*100, errorCount, len(results))
	t.Logf("Performance: P50=%v, P90=%v, P99=%v", p50, p90, p99)

	t.Logf("Upstream usage:")
	for upstream, count := range upstreamCounts {
		if count > 0 {
			percent := float64(count) * 100 / float64(totalSuccessful)
			t.Logf("  %s: %d calls (%.1f%%)", upstream, count, percent)
		}
	}

	// Log mock server stats
	t.Logf("Mock server stats:")
	for _, server := range mockServers {
		requests, errors := server.Stats()
		t.Logf("  %s: %d total requests, %d errors", server.ID, requests, errors)
	}

	// Validate expectations
	if scenario.ExpectedP50 > 0 {
		assert.Less(t, p50, scenario.ExpectedP50,
			"P50 (%v) should be less than %v", p50, scenario.ExpectedP50)
	}

	if scenario.ExpectedP90 > 0 {
		assert.Less(t, p90, scenario.ExpectedP90,
			"P90 (%v) should be less than %v", p90, scenario.ExpectedP90)
	}

	if scenario.MaxErrorRate > 0 {
		assert.LessOrEqual(t, errorRate, scenario.MaxErrorRate,
			"Error rate (%.1f%%) should be less than %.1f%%", errorRate*100, scenario.MaxErrorRate*100)
	}

	// Per-upstream distribution expectations
	if totalSuccessful > 0 {
		for _, ub := range scenario.Upstreams {
			pct := float64(upstreamCounts[ub.ID]) * 100 / float64(totalSuccessful)
			if ub.MinPercent > 0 {
				assert.GreaterOrEqual(t, pct, ub.MinPercent,
					"Upstream %s should handle at least %.1f%% of traffic but handled %.1f%%",
					ub.ID, ub.MinPercent, pct)
			}
			if ub.MaxPercent > 0 {
				assert.LessOrEqual(t, pct, ub.MaxPercent,
					"Upstream %s should handle at most %.1f%% of traffic but handled %.1f%%",
					ub.ID, ub.MaxPercent, pct)
			}
		}
	}

	// Expected final score order assertion (use captured score order)
	if len(scenario.ExpectedFinalScoreOrder) > 0 && len(finalOrder) > 0 {
		exp := scenario.ExpectedFinalScoreOrder
		obs := finalOrder
		if len(obs) >= len(exp) {
			obs = obs[:len(exp)]
		}
		if len(exp) > len(obs) {
			exp = exp[:len(obs)]
		}
		assert.Equal(t, exp, obs, "final score order should match expected")
	}

	t.Logf("=====================================\n")
}

// truncate returns s limited to maxLen with ellipsis if needed
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func scaledNumRequests(t *testing.T, base int) int {
	if testing.Short() {
		return minInt(base, TEST_DEFAULT_BATCH_SIZE)
	}
	if isRaceEnabled() {
		return minInt(base, TEST_DEFAULT_BATCH_SIZE*2)
	}
	return base
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
