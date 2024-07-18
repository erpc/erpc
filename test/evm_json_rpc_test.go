package test

// func TestStress_EvmJsonRpc(t *testing.T) {
// 	config := StressTestConfig{
// 		ServicePort: 4201,
// 		MetricsPort: 5201,
// 		ServerConfigs: []ServerConfig{
// 			{Port: 8081, FailureRate: 0.1, MinDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
// 			{Port: 8082, FailureRate: 0.2, MinDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
// 			{Port: 8083, FailureRate: 0.05, MinDelay: 30 * time.Millisecond, MaxDelay: 150 * time.Millisecond, SampleFile: "samples/evm-json-rpc.json"},
// 		},
// 		Duration: "30s",
// 		VUs:      10,
// 		MaxRPS:   2,
// 		AdditionalNetworkConfig: &common.NetworkConfig{
// 			Failsafe: &common.FailsafeConfig{
// 				Retry: &common.RetryPolicyConfig{
// 					MaxAttempts: 5,
// 					Delay:       "1000ms",
// 					Jitter:      "200ms",
// 				},
// 			},
// 		},
// 	}

// 	result, err := executeStressTest(config)
// 	if err != nil {
// 		t.Fatalf("Stress test failed: %v", err)
// 	}

// 	sums := result.SumCounter("erpc_network_requests_received_total", []string{})
// 	for _, sum := range sums {
// 		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("-------------erpc_network_requests_received_total")
// 	}
// 	sums = result.SumCounter("erpc_network_successful_requests_total", []string{})
// 	for _, sum := range sums {
// 		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("-------------erpc_network_successful_requests_total")
// 	}
// 	sums = result.SumCounter("erpc_network_failed_requests_total", []string{})
// 	for _, sum := range sums {
// 		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("-------------erpc_network_failed_requests_total")
// 	}
// 	sums = result.SumCounter("erpc_upstream_request_errors_total", []string{"upstream"})
// 	for _, sum := range sums {
// 		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("-------------erpc_upstream_request_errors_total")
// 	}
// 	sums = result.SumCounter("erpc_upstream_request_total", []string{"upstream"})
// 	for _, sum := range sums {
// 		log.Debug().Str("name", sum.Name).Interface("metric", sum).Msg("-------------erpc_upstream_request_total")
// 	}
// }
