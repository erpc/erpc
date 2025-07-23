package common

import (
	"net/url"
	"testing"
	"time"

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
			Failsafe: []*FailsafeConfig{
				{
					Timeout: &TimeoutPolicyConfig{
						Duration: Duration(100 * time.Millisecond),
					},
				},
			},
		})

		assert.NotNil(t, network.Failsafe)
		assert.Len(t, network.Failsafe, 1)
		assert.EqualValues(t, &FailsafeConfig{
			Timeout: &TimeoutPolicyConfig{
				Duration: Duration(100 * time.Millisecond),
			},
		}, network.Failsafe[0])
		assert.Nil(t, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].CircuitBreaker)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("NetworkDefaultsDefinesHedgeNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					Hedge: &HedgePolicyConfig{
						Delay:    Duration(100 * time.Millisecond),
						MaxCount: 10,
					},
				},
			},
		})

		assert.NotNil(t, network.Failsafe)
		assert.Len(t, network.Failsafe, 1)
		assert.EqualValues(t, &HedgePolicyConfig{
			Delay:    Duration(100 * time.Millisecond),
			MaxCount: 10,
		}, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].Timeout)
		assert.Nil(t, network.Failsafe[0].CircuitBreaker)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("NetworkDefaultsDefinesCircuitBreakerNoUserDefinedFailsafe", func(t *testing.T) {
		network := &NetworkConfig{}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					CircuitBreaker: &CircuitBreakerPolicyConfig{
						FailureThresholdCount: 10,
					},
				},
			},
		})

		assert.NotNil(t, network.Failsafe)
		assert.Len(t, network.Failsafe, 1)
		assert.EqualValues(t, &CircuitBreakerPolicyConfig{
			FailureThresholdCount: 10,
		}, network.Failsafe[0].CircuitBreaker)
		assert.Nil(t, network.Failsafe[0].Timeout)
		assert.Nil(t, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("UserDefinedRetryFailsafeWithoutNetworkDefaults", func(t *testing.T) {
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					Retry: &RetryPolicyConfig{
						MaxAttempts: 12345,
					},
				},
			},
		}
		network.SetDefaults(nil, nil)

		assert.EqualValues(t, &FailsafeConfig{
			MatchMethod: "*",
			Retry: &RetryPolicyConfig{
				MaxAttempts:     12345,
				Delay:           Duration(0 * time.Millisecond),
				BackoffMaxDelay: Duration(3 * time.Second),
				BackoffFactor:   1.2,
				Jitter:          Duration(0 * time.Millisecond),
			},
		}, network.Failsafe[0])
		assert.Nil(t, network.Failsafe[0].Timeout)
		assert.Nil(t, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].CircuitBreaker)
	})

	t.Run("UserDefinedTimeoutOverridesNetworkDefaults", func(t *testing.T) {
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					Timeout: &TimeoutPolicyConfig{
						Duration: Duration(5 * time.Second),
					},
				},
			},
		}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					Timeout: &TimeoutPolicyConfig{
						Duration: Duration(10 * time.Second),
					},
				},
			},
		})

		assert.EqualValues(t, "5s", network.Failsafe[0].Timeout.Duration.String(), "User-defined timeout should take precedence")
		assert.Nil(t, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].CircuitBreaker)
		assert.Nil(t, network.Failsafe[0].Retry)
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
		err := cfg.SetDefaults(&DefaultOptions{})
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

	t.Run("OnlyProviderShouldValidateSuccessfully", func(t *testing.T) {
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

		err := cfg.SetDefaults(&DefaultOptions{})
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

	t.Run("IndividualAndGlobalUpstreamDefaultsShouldBeAppliedAndValidatedSuccessfully", func(t *testing.T) {
		cfg := &Config{
			Projects: []*ProjectConfig{
				{
					Id: "test",
					Upstreams: []*UpstreamConfig{
						{
							Endpoint: "http://rpc1.localhost",
							// Individual upstream failsafe
							Failsafe: []*FailsafeConfig{
								{
									Retry: &RetryPolicyConfig{
										BackoffMaxDelay: Duration(10 * time.Second),
										Delay:           Duration(1 * time.Second),
										Jitter:          Duration(500 * time.Millisecond),
										MaxAttempts:     2,
										BackoffFactor:   1.2,
									},
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
						Failsafe: []*FailsafeConfig{
							{
								CircuitBreaker: &CircuitBreakerPolicyConfig{
									FailureThresholdCapacity: 200,
									FailureThresholdCount:    1,
									HalfOpenAfter:            Duration(5 * time.Minute),
									SuccessThresholdCapacity: 3,
									SuccessThresholdCount:    3,
								},
							},
						},
					},
				},
			},
		}

		// Apply defaults
		err := cfg.SetDefaults(&DefaultOptions{})
		assert.Nil(t, err, "SetDefaults should not return an error")

		// Verify failsafe retry is only applied to the first upstream
		retry := cfg.Projects[0].Upstreams[0].Failsafe[0].Retry
		assert.EqualValues(t, &RetryPolicyConfig{
			MaxAttempts:     2,
			BackoffMaxDelay: Duration(10 * time.Second),
			Delay:           Duration(1 * time.Second),
			Jitter:          Duration(500 * time.Millisecond),
			BackoffFactor:   1.2,
		}, retry, "Retry policy should match expected values")

		// Verify that upstreamDefaults circuit breaker failsafe is applied to all upstreams
		for _, upstream := range cfg.Projects[0].Upstreams {
			assert.Equal(t, []string{"eth_getLogs"}, upstream.AllowMethods)
			assert.NotNil(t, upstream.Failsafe)

			expectedCircuitBreaker := &CircuitBreakerPolicyConfig{
				FailureThresholdCapacity: 200,
				FailureThresholdCount:    1,
				HalfOpenAfter:            Duration(5 * time.Minute),
				SuccessThresholdCapacity: 3,
				SuccessThresholdCount:    3,
			}
			assert.Equal(t, expectedCircuitBreaker, upstream.Failsafe[0].CircuitBreaker)
		}

		// Validate the project configuration
		err = cfg.Validate()
		assert.Nil(t, err, "Validate should pass when providers and upstreams with defaults are present")
	})
}

func TestBuildProviderSettings(t *testing.T) {
	// Test case for Chainstack with query parameters
	t.Run("chainstack with filters", func(t *testing.T) {
		endpoint, _ := url.Parse("chainstack://test-api-key?project=proj-123&organization=org-456&region=us-east-1&provider=aws&type=dedicated")
		settings, err := buildProviderSettings("chainstack", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", settings["apiKey"])
		assert.Equal(t, "proj-123", settings["project"])
		assert.Equal(t, "org-456", settings["organization"])
		assert.Equal(t, "us-east-1", settings["region"])
		assert.Equal(t, "aws", settings["provider"])
		assert.Equal(t, "dedicated", settings["type"])
	})

	t.Run("chainstack with partial filters", func(t *testing.T) {
		endpoint, _ := url.Parse("chainstack://test-api-key?project=proj-123&type=shared")
		settings, err := buildProviderSettings("chainstack", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", settings["apiKey"])
		assert.Equal(t, "proj-123", settings["project"])
		assert.Equal(t, "shared", settings["type"])
		assert.Nil(t, settings["organization"])
		assert.Nil(t, settings["region"])
		assert.Nil(t, settings["provider"])
	})

	t.Run("chainstack without filters", func(t *testing.T) {
		endpoint, _ := url.Parse("chainstack://test-api-key")
		settings, err := buildProviderSettings("chainstack", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", settings["apiKey"])
		assert.Nil(t, settings["project"])
		assert.Nil(t, settings["organization"])
		assert.Nil(t, settings["region"])
		assert.Nil(t, settings["provider"])
		assert.Nil(t, settings["type"])
	})
}
