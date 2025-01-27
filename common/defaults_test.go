package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetDefaults_NetworkConfig(t *testing.T) {
	sysDefCfg := NewDefaultNetworkConfig(nil)

	t.Run("NoNetworkDefaultsAndNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, nil)

		assert.Nil(t, network.Failsafe, "Failsafe should be nil")
		assert.EqualValues(t, sysDefCfg.Failsafe, network.Failsafe)
	})

	t.Run("NetworkDefaultsDefinesFailsafeNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: &FailsafeConfig{
				Timeout: &TimeoutPolicyConfig{
					Duration: "100ms",
				},
			},
		})

		assert.EqualValues(t, &FailsafeConfig{
			Timeout: &TimeoutPolicyConfig{
				Duration: "100ms",
			},
		}, network.Failsafe)
		assert.Nil(t, network.Failsafe.Hedge)
		assert.Nil(t, network.Failsafe.CircuitBreaker)
		assert.Nil(t, network.Failsafe.Retry)
	})

	t.Run("NetworkDefaultsDefinesHedgeNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: &FailsafeConfig{
				Hedge: &HedgePolicyConfig{
					Delay:    "100ms",
					MaxCount: 10,
				},
			},
		})

		assert.EqualValues(t, &HedgePolicyConfig{
			Delay:    "100ms",
			MaxCount: 10,
		}, network.Failsafe.Hedge)
		assert.Nil(t, network.Failsafe.Timeout)
		assert.Nil(t, network.Failsafe.CircuitBreaker)
		assert.Nil(t, network.Failsafe.Retry)
	})

	t.Run("NetworkDefaultsDefinesCircuitBreakerNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: &FailsafeConfig{
				CircuitBreaker: &CircuitBreakerPolicyConfig{
					FailureThresholdCount: 10,
				},
			},
		})

		assert.EqualValues(t, &CircuitBreakerPolicyConfig{
			FailureThresholdCount: 10,
		}, network.Failsafe.CircuitBreaker)
		assert.Nil(t, network.Failsafe.Timeout)
		assert.Nil(t, network.Failsafe.Hedge)
		assert.Nil(t, network.Failsafe.Retry)
	})

	t.Run("UserDefinedRetryFailsafeWithoutNetworkDefaults", func(t *testing.T) {
		network := &NetworkConfig{
			Failsafe: &FailsafeConfig{
				Retry: &RetryPolicyConfig{
					MaxAttempts: 12345,
				},
			},
		}
		network.SetDefaults(nil, nil)

		assert.EqualValues(t, &FailsafeConfig{
			Retry: &RetryPolicyConfig{
				MaxAttempts:     12345,
				Delay:           "100ms",
				BackoffMaxDelay: "3s",
				BackoffFactor:   1.2,
				Jitter:          "0ms",
			},
		}, network.Failsafe)
		assert.Nil(t, network.Failsafe.Timeout)
		assert.Nil(t, network.Failsafe.Hedge)
		assert.Nil(t, network.Failsafe.CircuitBreaker)
	})

	t.Run("UserDefinedTimeoutOverridesNetworkDefaults", func(t *testing.T) {
		network := &NetworkConfig{
			Failsafe: &FailsafeConfig{
				Timeout: &TimeoutPolicyConfig{
					Duration: "5s",
				},
			},
		}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: &FailsafeConfig{
				Timeout: &TimeoutPolicyConfig{
					Duration: "10s",
				},
			},
		})

		assert.EqualValues(t, "5s", network.Failsafe.Timeout.Duration, "User-defined timeout should take precedence")
		assert.Nil(t, network.Failsafe.Hedge)
		assert.Nil(t, network.Failsafe.CircuitBreaker)
		assert.Nil(t, network.Failsafe.Retry)
	})
}

func TestSetDefaults_UpstreamConfig(t *testing.T) {
	t.Run("SchemeBasedUpstreamConfigConversionToProvider", func(t *testing.T) {
		cfg := &Config{
			Projects: []*ProjectConfig{
				{
					Id: "test1",
					Upstreams: []*UpstreamConfig{
						{
							Endpoint: "http://rpc1.localhost",
						},
						{
							Endpoint: "alchemy://some_test_api",
						},
						{
							Endpoint: "http://rpc3.localhost",
						},
					},
				},
			},
		}
		err := cfg.SetDefaults()
		assert.Nil(t, err)
		assert.Len(t, cfg.Projects[0].Upstreams, 2)
		assert.Len(t, cfg.Projects[0].Providers, 1)
		assert.EqualValues(t, "alchemy", cfg.Projects[0].Providers[0].Vendor)
		assert.ObjectsAreEqual(map[string]string{
			"apiKey": "some_test_api",
		}, cfg.Projects[0].Providers[0].Settings)
		assert.EqualValues(t, "http://rpc1.localhost", cfg.Projects[0].Upstreams[0].Endpoint)
		assert.EqualValues(t, "http://rpc3.localhost", cfg.Projects[0].Upstreams[1].Endpoint)
	})

	t.Run("Project with only provider as upstream should validate successfully", func(t *testing.T) {
		cfg := &Config{
			Projects: []*ProjectConfig{
				{
					Id: "test-alchemy-only",
					Upstreams: []*UpstreamConfig{
						{
							Endpoint: "alchemy://some_test_api_key",
						},
					},
				},
			},
		}

		err := cfg.SetDefaults()
		assert.Nil(t, err, "SetDefaults should not return an error")

		// Verify that the alchemy upstream has been converted to a provider
		project := cfg.Projects[0]
		assert.Len(t, project.Upstreams, 0, "Upstreams should be empty after converting alchemy upstream to provider")
		assert.Len(t, project.Providers, 1, "Providers should contain one provider after conversion")

		// Verify the provider's details
		provider := project.Providers[0]
		assert.Equal(t, "alchemy", provider.Vendor, "Provider vendor should be 'alchemy'")
		expectedSettings := VendorSettings{
			"apiKey": "some_test_api_key",
		}
		assert.Equal(t, expectedSettings, provider.Settings, "Provider settings should match expected values")

		// Validate the configuration
		err = project.Validate(cfg)
		assert.Nil(t, err, "Validate should pass when only a provider is present")
	})

	t.Run("Projects with individual and global upstream defaults should be applied and validated successfully", func(t *testing.T) {
		cfg := &Config{
			Projects: []*ProjectConfig{
				{
					Id: "test",
					Upstreams: []*UpstreamConfig{
						{
							Endpoint: "http://rpc1.localhost",
							// Individual upstream failsafe
							Failsafe: &FailsafeConfig{
								Retry: &RetryPolicyConfig{
									BackoffMaxDelay: "10s",
									Delay:           "1000ms",
									Jitter:          "500ms",
									MaxAttempts:     2,
									BackoffFactor:   1.2,
								},
							},
						},
						{
							Endpoint: "http://rpc2.localhost",
						},
					},
					// Global upstream failsafe defaults
					UpstreamDefaults: &UpstreamConfig{
						AllowMethods: []string{"eth_getLogs"},
						Failsafe: &FailsafeConfig{
							CircuitBreaker: &CircuitBreakerPolicyConfig{
								FailureThresholdCapacity: 200,
								FailureThresholdCount:    1,
								HalfOpenAfter:            "5m",
								SuccessThresholdCapacity: 3,
								SuccessThresholdCount:    3,
							},
						},
					},
				},
			},
		}

		// Apply defaults
		err := cfg.SetDefaults()
		assert.Nil(t, err, "SetDefaults should not return an error")

		// Verify failsafe retry is only applied to the first upstream
		retry := cfg.Projects[0].Upstreams[0].Failsafe.Retry
		assert.EqualValues(t, &RetryPolicyConfig{
			MaxAttempts:     2,
			BackoffMaxDelay: "10s",
			Delay:           "1000ms",
			Jitter:          "500ms",
			BackoffFactor:   1.2,
		}, retry, "Retry policy should match expected values")

		// Verify that upstreamDefaults circuit breaker failsafe is applied to all upstreams
		for _, upstream := range cfg.Projects[0].Upstreams {
			assert.Equal(t, []string{"eth_getLogs"}, upstream.AllowMethods)
			assert.NotNil(t, upstream.Failsafe)

			expectedCircuitBreaker := &CircuitBreakerPolicyConfig{
				FailureThresholdCapacity: 200,
				FailureThresholdCount:    1,
				HalfOpenAfter:            "5m",
				SuccessThresholdCapacity: 3,
				SuccessThresholdCount:    3,
			}
			assert.Equal(t, expectedCircuitBreaker, upstream.Failsafe.CircuitBreaker)
		}

		// Validate the project configuration
		err = cfg.Validate()
		assert.Nil(t, err, "Validate should pass when providers and upstreams with defaults are present")
	})
}
