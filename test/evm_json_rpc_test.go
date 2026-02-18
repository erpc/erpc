package test

import (
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

func TestStress_EvmJsonRpc_SimpleVariedFailures(t *testing.T) {
	if os.Getenv("RUN_K6_STRESS_TESTS") != "1" {
		t.Skip("set RUN_K6_STRESS_TESTS=1 to run k6 stress tests")
	}

	if _, err := exec.LookPath("k6"); err != nil {
		t.Skip("k6 binary is required for stress test")
	}

	// Override OsExit to prevent erpc.Init goroutine from killing the test process
	// when the HTTP server shuts down during test cleanup.
	origExit := util.OsExit
	util.OsExit = func(code int) {
		t.Logf("util.OsExit(%d) intercepted during test", code)
	}
	t.Cleanup(func() { util.OsExit = origExit })

	servicePort := mustFreeTCPPort(t)
	metricsPort := mustFreeTCPPort(t)
	serverPort1 := mustFreeTCPPort(t)
	serverPort2 := mustFreeTCPPort(t)
	serverPort3 := mustFreeTCPPort(t)
	config := StressTestConfig{
		ServicePort: servicePort,
		MetricsPort: metricsPort,
		ServerConfigs: []ServerConfig{
			{Port: serverPort1, FailureRate: 0.1, MinDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
			{Port: serverPort2, FailureRate: 0.2, MinDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
			{Port: serverPort3, FailureRate: 0.05, MinDelay: 30 * time.Millisecond, MaxDelay: 150 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
		},
		Duration: "60s",
		VUs:      50,
		MaxRPS:   10000,
		AdditionalNetworkConfig: &common.NetworkConfig{
			Failsafe: []*common.FailsafeConfig{{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 4,
					Delay:       common.Duration(1000 * time.Millisecond),
					Jitter:      common.Duration(200 * time.Millisecond),
				},
			}},
		},
	}

	result, err := executeStressTest(config)
	if err != nil {
		t.Fatalf("Stress test failed: %v", err)
	}

	totalNetworkRequests := 0
	sums := result.SumCounter("erpc_network_request_received_total", []string{})
	for _, sum := range sums {
		totalNetworkRequests += int(sum.Value)
	}

	totalNetworkSuccess := 0
	sums = result.SumCounter("erpc_network_successful_request_total", []string{})
	for _, sum := range sums {
		totalNetworkSuccess += int(sum.Value)
	}
	if totalNetworkSuccess < totalNetworkRequests {
		t.Fatalf("Network-level success is less than network requests: %d total success < %d total requests", totalNetworkSuccess, totalNetworkRequests)
	}

	totalNetworkErrors := 0.0
	sums = result.SumCounter("erpc_network_failed_request_total", []string{"errorType"})
	for _, sum := range sums {
		totalNetworkErrors += sum.Value
	}
	if totalNetworkErrors > 0 {
		t.Fatalf("Network-level errors recorded which is not expected: %f", totalNetworkErrors)
	}

	totalUpstreamRequests := 0.0
	sums = result.SumCounter("erpc_upstream_request_total", []string{"upstream"})
	for _, sum := range sums {
		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("erpc_upstream_request_total")
		totalUpstreamRequests += sum.Value
	}
	if totalUpstreamRequests == 0 {
		t.Fatalf("No upstream requests recorded which is not expected: %f", totalUpstreamRequests)
	}

	totalUpstreamErrors := 0.0
	sums = result.SumCounter("erpc_upstream_request_errors_total", []string{"upstream", "errorType"})
	for _, sum := range sums {
		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("erpc_upstream_request_errors_total")
		totalUpstreamErrors += sum.Value
	}
	if totalUpstreamErrors == 0 {
		t.Fatalf("No upstream errors recorded which is not expected: %f", totalUpstreamErrors)
	}
}

func mustFreeTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate test TCP port: %v", err)
	}
	defer l.Close()

	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type: %T", l.Addr())
	}

	return addr.Port
}
