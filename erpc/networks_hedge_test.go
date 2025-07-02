package erpc

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
)

func TestNetworksHedge(t *testing.T) {
	t.Run("FixedDelayHedge", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									MatchMethod: "eth_getLogs",
									Hedge: &common.HedgePolicyConfig{
										Delay:    common.Duration(500 * time.Millisecond), // Fixed delay
										MaxCount: 1,
									},
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(10 * time.Second),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "primary",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://primary.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
						{
							Id:       "secondary",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://secondary.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Track request times
		var mu sync.Mutex
		requestTimes := make(map[string]time.Time)

		// Mock primary - slow
		gock.New("http://primary.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				if strings.Contains(body, `"method":"eth_getLogs"`) {
					mu.Lock()
					requestTimes["primary"] = time.Now()
					mu.Unlock()
					return true
				}
				return false
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []interface{}{
					map[string]interface{}{
						"address": "0x1234",
						"data":    "primary_response",
					},
				},
			}).
			Delay(2 * time.Second)

		// Mock secondary - fast
		gock.New("http://secondary.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				if strings.Contains(body, `"method":"eth_getLogs"`) {
					mu.Lock()
					requestTimes["secondary"] = time.Now()
					mu.Unlock()
					return true
				}
				return false
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []interface{}{
					map[string]interface{}{
						"address": "0x5678",
						"data":    "secondary_response",
					},
				},
			}).
			Delay(100 * time.Millisecond)

		// Send request
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(
			`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1000","toBlock":"0x1100"}],"id":1}`,
			nil,
			nil,
		)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "secondary_response", "Should return faster response")

		// Verify hedge timing
		mu.Lock()
		primaryTime, hasPrimary := requestTimes["primary"]
		secondaryTime, hasSecondary := requestTimes["secondary"]
		mu.Unlock()

		if hasPrimary && hasSecondary {
			hedgeDelay := secondaryTime.Sub(primaryTime)
			t.Logf("Actual hedge delay: %v (expected ~500ms)", hedgeDelay)

			// Allow 10ms tolerance
			assert.InDelta(t, 500, hedgeDelay.Milliseconds(), 10,
				"Fixed delay hedge should work correctly")
		}
	})

	t.Run("QuantileBasedHedge", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									MatchMethod: "eth_getLogs",
									Hedge: &common.HedgePolicyConfig{
										Quantile: 0.95,
										MaxCount: 1,
										MinDelay: common.Duration(100 * time.Millisecond),
										MaxDelay: common.Duration(10 * time.Second),
									},
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(30 * time.Second),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "upstream1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://upstream1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
						{
							Id:       "upstream2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://upstream2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Create server fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Pre-populate metrics by sending several requests first
		t.Log("Pre-populating metrics data...")
		for i := 0; i < 20; i++ {
			gock.New("http://upstream1.localhost").
				Post("/").
				Filter(func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					return strings.Contains(body, `"method":"eth_getLogs"`)
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				}).
				Delay(150 * time.Millisecond)

			gock.New("http://upstream2.localhost").
				Post("/").
				Filter(func(request *http.Request) bool {
					body := util.SafeReadBody(request)
					return strings.Contains(body, `"method":"eth_getLogs"`)
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  []interface{}{},
				}).
				Delay(150 * time.Millisecond)

			// Send request to populate metrics
			sendRequest(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x%x","toBlock":"0x%x"}],"id":%d}`,
					100+i, 200+i, i),
				nil,
				nil,
			)
			time.Sleep(200 * time.Millisecond)
		}

		// Now test hedge behavior with populated metrics
		var mu sync.Mutex
		requestTimes := make(map[string]time.Time)

		// Reset gock for actual test
		util.ResetGock()

		// Mock upstream1 - slow
		gock.New("http://upstream1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				if strings.Contains(body, `"method":"eth_getLogs"`) && strings.Contains(body, `"toBlock":"0x2000"`) {
					mu.Lock()
					requestTimes["upstream1"] = time.Now()
					mu.Unlock()
					return true
				}
				return false
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			}).
			Delay(5 * time.Second)

		// Mock upstream2 - fast
		gock.New("http://upstream2.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				if strings.Contains(body, `"method":"eth_getLogs"`) && strings.Contains(body, `"toBlock":"0x2000"`) {
					mu.Lock()
					requestTimes["upstream2"] = time.Now()
					mu.Unlock()
					return true
				}
				return false
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []interface{}{
					map[string]interface{}{
						"data": "upstream2_response",
					},
				},
			}).
			Delay(100 * time.Millisecond)

		// Send the actual test request
		t.Log("Sending test request with populated metrics...")
		statusCode, body := sendRequest(
			`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1000","toBlock":"0x2000"}],"id":1}`,
			nil,
			nil,
		)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "upstream2_response")

		// Check hedge timing
		mu.Lock()
		time1, has1 := requestTimes["upstream1"]
		time2, has2 := requestTimes["upstream2"]
		mu.Unlock()

		if has1 && has2 {
			hedgeDelay := time2.Sub(time1)
			if time1.After(time2) {
				hedgeDelay = time1.Sub(time2)
			}

			t.Logf("Hedge delay with populated metrics: %v", hedgeDelay)

			// With metrics showing ~150ms P95, the hedge delay should be at least minDelay (100ms)
			if hedgeDelay < 100*time.Millisecond {
				t.Errorf("Even with populated metrics, hedge delay %v is less than minDelay of 100ms", hedgeDelay)
			} else {
				t.Logf("✓ With populated metrics, hedge delay respects minDelay!")
			}
		} else {
			if !has1 && has2 {
				t.Log("Only upstream2 received request - possible that upstream1 was not selected")
			} else if has1 && !has2 {
				t.Log("Only upstream1 received request - hedge might not have triggered")
			} else {
				t.Error("Neither upstream received the test request")
			}
		}
	})

}
