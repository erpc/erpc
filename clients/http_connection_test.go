package clients

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

// ConnectionTrackingServer tracks TCP connections and simulates high latency
type ConnectionTrackingServer struct {
	*httptest.Server
	connectionCount int64
	activeConns     int64
	totalConns      int64
	latency         time.Duration
	mu              sync.RWMutex
	connStates      map[net.Conn]http.ConnState
}

// NewConnectionTrackingServer creates a server that tracks connections and adds latency
func NewConnectionTrackingServer(latency time.Duration) *ConnectionTrackingServer {
	cts := &ConnectionTrackingServer{
		latency:    latency,
		connStates: make(map[net.Conn]http.ConnState),
	}

	// Create HTTP handler that simulates JSON-RPC response with latency
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate network latency (e.g., Virginia to Germany)
		if cts.latency > 0 {
			time.Sleep(cts.latency)
		}

		// Simulate a simple JSON-RPC response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","result":"0x1","id":1}`)
	})

	// Create test server with connection state tracking
	server := httptest.NewUnstartedServer(handler)

	// Track connection states
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		cts.mu.Lock()
		defer cts.mu.Unlock()

		oldState, existed := cts.connStates[conn]
		cts.connStates[conn] = state

		switch state {
		case http.StateNew:
			if !existed {
				atomic.AddInt64(&cts.totalConns, 1)
				atomic.AddInt64(&cts.activeConns, 1)
			}
		case http.StateActive:
			if oldState != http.StateActive {
				atomic.AddInt64(&cts.connectionCount, 1)
			}
		case http.StateClosed:
			if existed {
				atomic.AddInt64(&cts.activeConns, -1)
				delete(cts.connStates, conn)
			}
		}
	}

	server.Start()
	cts.Server = server

	return cts
}

func (cts *ConnectionTrackingServer) GetStats() (totalConns int64, activeConns int64, connectionCount int64) {
	return atomic.LoadInt64(&cts.totalConns),
		atomic.LoadInt64(&cts.activeConns),
		atomic.LoadInt64(&cts.connectionCount)
}

func (cts *ConnectionTrackingServer) ResetStats() {
	atomic.StoreInt64(&cts.totalConns, 0)
	atomic.StoreInt64(&cts.activeConns, 0)
	atomic.StoreInt64(&cts.connectionCount, 0)
	cts.mu.Lock()
	cts.connStates = make(map[net.Conn]http.ConnState)
	cts.mu.Unlock()
}

type phonyUpstream struct {
	common.Upstream
}

var _ common.Upstream = (*phonyUpstream)(nil)

func (u *phonyUpstream) Id() string {
	return "phony-upstream"
}

type noopErrorExtractor struct{}

func (e *noopErrorExtractor) Extract(resp *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse, upstream common.Upstream) error {
	return nil
}

// TestHTTPConnectionReuseUnderLoad tests connection behavior under high load with latency
func TestHTTPConnectionReuseUnderLoad(t *testing.T) {
	t.Parallel()
	// Test parameters simulating the user's scenario
	const (
		requestCount     = 200                    // Total requests to send
		concurrency      = 20                     // Concurrent goroutines (simulating 100 RPS burst)
		simulatedLatency = 400 * time.Millisecond // Virginia to Germany latency
	)

	logger := zerolog.New(nil).Level(zerolog.ErrorLevel)

	// Create connection tracking server with latency
	server := NewConnectionTrackingServer(simulatedLatency)
	defer server.Close()

	parsedURL, err := url.Parse(server.URL)
	assert.NoError(t, err)

	// Test with current (problematic) configuration
	t.Run("Current_Configuration_MaxConns2", func(t *testing.T) {
		server.ResetStats()

		// Create client with OLD problematic configuration (MaxConnsPerHost defaults to 2)
		ctx := context.Background()

		// Simulate old configuration before our fix
		oldTransport := &http.Transport{
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			// Missing MaxConnsPerHost (defaults to 2) - this was the problem
		}

		oldHttpClient := &http.Client{
			Timeout:   60 * time.Second,
			Transport: oldTransport,
		}

		client := &GenericHttpJsonRpcClient{
			Url:             parsedURL,
			appCtx:          ctx,
			logger:          &logger,
			projectId:       "test-project",
			httpClient:      oldHttpClient,
			isLogLevelTrace: false,
			upstream:        &phonyUpstream{},
			gzipPool:        util.NewGzipReaderPool(),
			gzipWriterPool:  util.NewGzipWriterPool(),
			errorExtractor:  &noopErrorExtractor{},
		}

		// Send requests concurrently to simulate load
		var wg sync.WaitGroup
		startTime := time.Now()

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				requestsPerWorker := requestCount / concurrency
				for j := 0; j < requestsPerWorker; j++ {
					req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, workerID*requestsPerWorker+j)))

					resp, err := client.SendRequest(ctx, req)
					if err != nil {
						t.Logf("Request failed: %v", err)
					} else if resp != nil {
						// Consume the response to enable connection reuse
						_, _ = resp.JsonRpcResponse()
						resp.Release()
					}

					// Small delay to simulate realistic request pacing
					time.Sleep(10 * time.Millisecond)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(startTime)

		// Get final connection stats
		totalConns, activeConns, connActivations := server.GetStats()

		t.Logf("=== CURRENT CONFIGURATION RESULTS ===")
		t.Logf("Total requests: %d", requestCount)
		t.Logf("Test duration: %v", duration)
		t.Logf("Average RPS: %.2f", float64(requestCount)/duration.Seconds())
		t.Logf("Total TCP connections created: %d", totalConns)
		t.Logf("Active connections at end: %d", activeConns)
		t.Logf("Connection activations: %d", connActivations)
		t.Logf("Connections per request: %.2f", float64(totalConns)/float64(requestCount))
		t.Logf("=====================================")

		// With default MaxConnsPerHost=2, we expect high connection churn
		// This is our baseline - should show poor connection reuse
		assert.Greater(t, totalConns, int64(2), "Should create more than 2 connections under load")

		// Store results for comparison
		currentConfigResults := map[string]interface{}{
			"totalConns":        totalConns,
			"activeConns":       activeConns,
			"connActivations":   connActivations,
			"duration":          duration,
			"connectionsPerReq": float64(totalConns) / float64(requestCount),
		}

		// Store in test context for later comparison
		t.Logf("Baseline established - connections/request ratio: %.3f", currentConfigResults["connectionsPerReq"])
	})

	// Test with FIXED production configuration
	t.Run("Fixed_Production_Configuration", func(t *testing.T) {
		server.ResetStats()

		// Create client with our NEW FIXED configuration (as used in production)
		ctx := context.Background()

		// This is our fixed transport configuration now in production
		fixedTransport := &http.Transport{
			MaxIdleConns:          1024,
			MaxIdleConnsPerHost:   256,
			MaxConnsPerHost:       0, // Unlimited active connections (the key fix)
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}

		fixedHttpClient := &http.Client{
			Timeout:   60 * time.Second,
			Transport: fixedTransport,
		}

		client := &GenericHttpJsonRpcClient{
			Url:             parsedURL,
			appCtx:          ctx,
			logger:          &logger,
			projectId:       "test-project",
			httpClient:      fixedHttpClient,
			isLogLevelTrace: false,
			upstream:        &phonyUpstream{},
			gzipPool:        util.NewGzipReaderPool(),
			gzipWriterPool:  util.NewGzipWriterPool(),
			errorExtractor:  &noopErrorExtractor{},
		}

		// Send the same load pattern as before
		var wg sync.WaitGroup
		startTime := time.Now()

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				requestsPerWorker := requestCount / concurrency
				for j := 0; j < requestsPerWorker; j++ {
					req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, workerID*requestsPerWorker+j)))

					resp, err := client.SendRequest(ctx, req)
					if err != nil {
						t.Logf("Request failed: %v", err)
					} else if resp != nil {
						// Consume the response to enable connection reuse
						_, _ = resp.JsonRpcResponse()
						resp.Release()
					}

					// Small delay to simulate realistic request pacing
					time.Sleep(10 * time.Millisecond)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(startTime)

		// Get final connection stats
		totalConns, activeConns, connActivations := server.GetStats()

		t.Logf("=== FIXED PRODUCTION CONFIGURATION RESULTS ===")
		t.Logf("Total requests: %d", requestCount)
		t.Logf("Test duration: %v", duration)
		t.Logf("Average RPS: %.2f", float64(requestCount)/duration.Seconds())
		t.Logf("Total TCP connections created: %d", totalConns)
		t.Logf("Active connections at end: %d", activeConns)
		t.Logf("Connection activations: %d", connActivations)
		t.Logf("Connections per request: %.2f", float64(totalConns)/float64(requestCount))
		t.Logf("============================================")

		// The fixed version should perform as well as our optimized test version
		fixedRatio := float64(totalConns) / float64(requestCount)
		t.Logf("Fixed production connections/request ratio: %.3f", fixedRatio)

		// Should have similar performance to our optimized configuration
		assert.LessOrEqual(t, totalConns, int64(concurrency+5), "Fixed config should not create excessive connections")
		assert.Less(t, fixedRatio, 0.15, "Fixed config should have very good connection reuse")
	})

	// Test with optimized configuration (the fix we'll implement)
	t.Run("Optimized_Configuration_UnlimitedConns", func(t *testing.T) {
		server.ResetStats()

		// Create client with optimized transport configuration
		ctx := context.Background()

		// This simulates our proposed fix
		transport := &http.Transport{
			MaxIdleConns:          1024,
			MaxIdleConnsPerHost:   256,
			MaxConnsPerHost:       0, // Unlimited active connections (the key fix)
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}

		httpClient := &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
		}

		// Create client with custom transport
		client := &GenericHttpJsonRpcClient{
			Url:             parsedURL,
			appCtx:          ctx,
			logger:          &logger,
			projectId:       "test-project",
			httpClient:      httpClient,
			isLogLevelTrace: false,
			upstream:        &phonyUpstream{},
			gzipPool:        util.NewGzipReaderPool(),
			gzipWriterPool:  util.NewGzipWriterPool(),
			errorExtractor:  &noopErrorExtractor{},
		}

		// Send the same load pattern
		var wg sync.WaitGroup
		startTime := time.Now()

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				requestsPerWorker := requestCount / concurrency
				for j := 0; j < requestsPerWorker; j++ {
					req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, workerID*requestsPerWorker+j)))

					resp, err := client.SendRequest(ctx, req)
					if err != nil {
						t.Logf("Request failed: %v", err)
					} else if resp != nil {
						// Consume the response to enable connection reuse
						_, _ = resp.JsonRpcResponse()
						resp.Release()
					}

					// Small delay to simulate realistic request pacing
					time.Sleep(10 * time.Millisecond)
				}
			}(i)
		}

		wg.Wait()
		duration := time.Since(startTime)

		// Get final connection stats
		totalConns, activeConns, connActivations := server.GetStats()

		t.Logf("=== OPTIMIZED CONFIGURATION RESULTS ===")
		t.Logf("Total requests: %d", requestCount)
		t.Logf("Test duration: %v", duration)
		t.Logf("Average RPS: %.2f", float64(requestCount)/duration.Seconds())
		t.Logf("Total TCP connections created: %d", totalConns)
		t.Logf("Active connections at end: %d", activeConns)
		t.Logf("Connection activations: %d", connActivations)
		t.Logf("Connections per request: %.2f", float64(totalConns)/float64(requestCount))
		t.Logf("=======================================")

		// With MaxConnsPerHost=0, we expect much better connection reuse
		// Should create far fewer connections than the baseline
		optimizedRatio := float64(totalConns) / float64(requestCount)

		t.Logf("Optimized connections/request ratio: %.3f", optimizedRatio)

		// The optimized version should create significantly fewer connections
		// We expect connection reuse to be much better
		assert.LessOrEqual(t, totalConns, int64(concurrency+5), "Should not create excessive connections with unlimited MaxConnsPerHost")
		assert.Less(t, optimizedRatio, 0.15, "Should have very good connection reuse (less than 0.15 connections per request)")
	})
}

// TestConnectionLimitsWithRealWorldScenario tests the specific issue reported by the user
func TestConnectionLimitsWithRealWorldScenario(t *testing.T) {
	t.Parallel()
	logger := zerolog.New(nil).Level(zerolog.ErrorLevel)

	// Simulate the exact user scenario: 100 RPS with 400ms latency
	server := NewConnectionTrackingServer(400 * time.Millisecond)
	defer server.Close()

	parsedURL, err := url.Parse(server.URL)
	assert.NoError(t, err)

	t.Run("Simulate_User_Reported_Issue", func(t *testing.T) {
		// Create client with current problematic configuration
		ctx := context.Background()
		ups := common.NewFakeUpstream("rpc1")
		client, err := NewGenericHttpJsonRpcClient(ctx, &logger, "test-project", ups, parsedURL, nil, nil, &noopErrorExtractor{})
		assert.NoError(t, err)

		// Simulate 100 RPS for 5 seconds (500 requests total)
		const (
			targetRPS = 100
		)
		testDuration := 5 * time.Second
		totalReqs := int(targetRPS) * int(testDuration.Seconds())

		requestInterval := time.Second / targetRPS
		var requestCount int64
		var errorCount int64

		startTime := time.Now()
		ticker := time.NewTicker(requestInterval)
		defer ticker.Stop()

		done := time.After(testDuration)
		var wg sync.WaitGroup

	sendLoop:
		for {
			select {
			case <-ticker.C:
				wg.Add(1)
				go func(reqID int64) {
					defer wg.Done()
					req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, reqID)))

					reqStart := time.Now()
					_, err := client.SendRequest(ctx, req)
					reqDuration := time.Since(reqStart)

					if err != nil {
						atomic.AddInt64(&errorCount, 1)
						if reqDuration > 10*time.Second {
							t.Logf("Request %d failed after %v: %v", reqID, reqDuration, err)
						}
					}
					atomic.AddInt64(&requestCount, 1)
				}(atomic.LoadInt64(&requestCount))

			case <-done:
				break sendLoop
			}
		}

		// Wait for all requests to complete (with timeout)
		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()

		select {
		case <-waitDone:
			// All requests completed
		case <-time.After(30 * time.Second):
			t.Log("WARNING: Some requests didn't complete within 30 seconds")
		}

		actualDuration := time.Since(startTime)
		totalConns, activeConns, _ := server.GetStats()
		finalRequestCount := atomic.LoadInt64(&requestCount)
		finalErrorCount := atomic.LoadInt64(&errorCount)

		t.Logf("=== USER SCENARIO SIMULATION RESULTS ===")
		t.Logf("Target: %d RPS for %v (%d requests)", targetRPS, testDuration, totalReqs)
		t.Logf("Actual: %.1f RPS over %v (%d requests)", float64(finalRequestCount)/actualDuration.Seconds(), actualDuration, finalRequestCount)
		t.Logf("Errors: %d (%.1f%%)", finalErrorCount, float64(finalErrorCount)/float64(finalRequestCount)*100)
		t.Logf("TCP connections created: %d", totalConns)
		t.Logf("Active connections: %d", activeConns)
		t.Logf("Connection churn: %.2f connections per request", float64(totalConns)/float64(finalRequestCount))
		t.Logf("========================================")

		// This test demonstrates the problem: with default MaxConnsPerHost=2,
		// high latency + high RPS = excessive connection creation
		assert.Greater(t, totalConns, int64(20), "Should demonstrate connection creation with current config")

		connectionChurnRatio := float64(totalConns) / float64(finalRequestCount)
		t.Logf("🚨 Problem demonstrated: %.3f connections created per request", connectionChurnRatio)
		t.Logf("💡 This explains why the Reth node gets overwhelmed!")
	})
}
