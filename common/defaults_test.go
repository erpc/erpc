package common

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// "Data not available yet" retries (empty/missing-data/block-unavailable) default
// to one original attempt + one retry, independent of MaxAttempts.
func TestRetryPolicyConfig_DefaultEmptyResultMaxAttempts(t *testing.T) {
	r := &RetryPolicyConfig{MaxAttempts: 6}
	require.NoError(t, r.SetDefaults(nil))
	assert.Equal(t, 2, r.EmptyResultMaxAttempts,
		"EmptyResultMaxAttempts should default to 2 (one retry), separate from MaxAttempts")
}

// Deprecated blockUnavailableDelay migrates into emptyResultDelay at config-load time
// only, then is cleared — old configs keep working without any runtime legacy.
func TestRetryPolicyConfig_MigratesDeprecatedBlockUnavailableDelay(t *testing.T) {
	r := &RetryPolicyConfig{MaxAttempts: 3, BlockUnavailableDelay: Duration(700 * time.Millisecond)}
	require.NoError(t, r.SetDefaults(nil))
	assert.Equal(t, Duration(700*time.Millisecond), r.EmptyResultDelay, "value migrated into emptyResultDelay")
	assert.Equal(t, Duration(0), r.BlockUnavailableDelay, "legacy field cleared after migration")

	// An explicitly-set emptyResultDelay is never clobbered by the legacy value.
	r2 := &RetryPolicyConfig{
		MaxAttempts:           3,
		EmptyResultDelay:      Duration(200 * time.Millisecond),
		BlockUnavailableDelay: Duration(700 * time.Millisecond),
	}
	require.NoError(t, r2.SetDefaults(nil))
	assert.Equal(t, Duration(200*time.Millisecond), r2.EmptyResultDelay, "explicit emptyResultDelay wins")
	assert.Equal(t, Duration(0), r2.BlockUnavailableDelay)
}

func boolPtr(b bool) *bool { return &b }

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
						Duration: NewStaticDuration(100 * time.Millisecond),
					},
				},
			},
		})

		assert.NotNil(t, network.Failsafe)
		assert.Len(t, network.Failsafe, 1)
		assert.EqualValues(t, &FailsafeConfig{
			Timeout: &TimeoutPolicyConfig{
				Duration: NewStaticDuration(100 * time.Millisecond),
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
						Delay:    NewStaticDuration(100 * time.Millisecond),
						MaxCount: 10,
					},
				},
			},
		})

		assert.NotNil(t, network.Failsafe)
		assert.Len(t, network.Failsafe, 1)
		assert.EqualValues(t, &HedgePolicyConfig{
			Delay:    NewStaticDuration(100 * time.Millisecond),
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
				MaxAttempts:            12345,
				Delay:                  Duration(0 * time.Millisecond),
				BackoffMaxDelay:        Duration(3 * time.Second),
				BackoffFactor:          1.2,
				Jitter:                 Duration(0 * time.Millisecond),
				EmptyResultAccept:      DefaultEmptyResultAccept(),
				EmptyResultMaxAttempts: 2,
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
						Duration: NewStaticDuration(5 * time.Second),
					},
				},
			},
		}
		network.SetDefaults(nil, &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		})

		assert.EqualValues(t, 5*time.Second, network.Failsafe[0].Timeout.Duration.Resolve(nil), "User-defined timeout should take precedence")
		assert.Nil(t, network.Failsafe[0].Hedge)
		assert.Nil(t, network.Failsafe[0].CircuitBreaker)
		assert.Nil(t, network.Failsafe[0].Retry)
	})
}

func TestServerConfigSetDefaults_GrpcPortDefaultsToHttpPort(t *testing.T) {
	server := &ServerConfig{
		HttpHostV4:  util.StringPtr("127.0.0.1"),
		HttpHostV6:  util.StringPtr("[::1]"),
		HttpPortV4:  util.IntPtr(4311),
		HttpPortV6:  util.IntPtr(5311),
		GrpcEnabled: util.BoolPtr(true),
	}

	err := server.SetDefaults()
	assert.NoError(t, err)
	assert.NotNil(t, server.GrpcHostV4)
	assert.NotNil(t, server.GrpcHostV6)
	assert.NotNil(t, server.GrpcPortV4)
	assert.NotNil(t, server.GrpcPortV6)
	assert.Equal(t, "127.0.0.1", *server.GrpcHostV4)
	assert.Equal(t, "[::1]", *server.GrpcHostV6)
	assert.Equal(t, 4311, *server.GrpcPortV4)
	assert.Equal(t, 5311, *server.GrpcPortV6)
	// gRPC server reflection defaults to enabled.
	assert.NotNil(t, server.GrpcReflection)
	assert.True(t, *server.GrpcReflection)
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

	t.Run("UpstreamFailsafeMatchMethodPreservedWhenNoMatchingDefault", func(t *testing.T) {
		// User defines failsafe for specific method, defaults define different method
		// User's matchMethod should NOT be overwritten
		upstream := &UpstreamConfig{
			Endpoint: "http://rpc1.localhost",
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &UpstreamConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := upstream.SetDefaults(defaults)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 1)
		// User's matchMethod should be preserved
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", upstream.Failsafe[0].MatchMethod)
		// User's timeout should be preserved
		assert.Equal(t, "10s", upstream.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Retry should NOT be applied (no match)
		assert.Nil(t, upstream.Failsafe[0].Retry)
	})

	t.Run("UpstreamFailsafeMatchingDefaultMergesConfig", func(t *testing.T) {
		// User and default have matching method/finality, config should merge
		upstream := &UpstreamConfig{
			Endpoint: "http://rpc1.localhost",
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &UpstreamConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := upstream.SetDefaults(defaults)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 1)
		assert.Equal(t, "eth_getLogs", upstream.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", upstream.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Retry should be applied from matching default
		assert.NotNil(t, upstream.Failsafe[0].Retry)
		assert.EqualValues(t, 5, upstream.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("UpstreamFailsafeNoDefaults_SystemDefaultsApplied", func(t *testing.T) {
		// No defaults provided, system defaults should apply
		upstream := &UpstreamConfig{
			Endpoint: "http://rpc1.localhost",
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := upstream.SetDefaults(nil)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 1)
		assert.Equal(t, "eth_getLogs", upstream.Failsafe[0].MatchMethod)
		assert.NotNil(t, upstream.Failsafe[0].Retry)
		// System defaults for retry should be applied
		assert.EqualValues(t, 5, upstream.Failsafe[0].Retry.MaxAttempts)
		assert.NotZero(t, upstream.Failsafe[0].Retry.BackoffFactor) // System default
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
			MaxAttempts:            2,
			BackoffMaxDelay:        Duration(10 * time.Second),
			Delay:                  Duration(1 * time.Second),
			Jitter:                 Duration(500 * time.Millisecond),
			BackoffFactor:          1.2,
			EmptyResultAccept:      DefaultEmptyResultAccept(),
			EmptyResultMaxAttempts: 2,
		}, retry, "Retry policy should match expected values")

		assert.Nil(t, cfg.Projects[0].Upstreams[0].Failsafe[0].CircuitBreaker, "Circuit breaker should be nil because this upstream has failsafe defined")

		// Validate the project configuration
		err = cfg.Validate()
		assert.Nil(t, err, "Validate should pass when providers and upstreams with defaults are present")
	})
}

func TestMethodsConfigStatefulMethodsWithPreserveDefaultsFalse(t *testing.T) {
	// Test case 1: Custom definitions with PreserveDefaultMethods=false
	// This should still mark default stateful methods as stateful
	m := &MethodsConfig{
		PreserveDefaultMethods: false,
		Definitions: map[string]*CacheMethodConfig{
			"custom_method": {
				Finalized: true,
			},
		},
	}

	err := m.SetDefaults()
	assert.NoError(t, err, "SetDefaults should not fail")

	// Verify that default stateful methods are marked as stateful
	for _, methodName := range DefaultStatefulMethodNames {
		method, exists := m.Definitions[methodName]
		assert.True(t, exists, "Default stateful method %s should exist in definitions", methodName)
		if exists {
			assert.True(t, method.Stateful, "Default stateful method %s should be marked as stateful", methodName)
		}
	}

	// Verify custom method still exists
	_, exists := m.Definitions["custom_method"]
	assert.True(t, exists, "Custom method 'custom_method' should still exist in definitions")
}

func TestMethodsConfigStatefulMethodsWithPreserveDefaultsTrue(t *testing.T) {
	// Test case 2: Custom definitions with PreserveDefaultMethods=true
	// This should preserve all defaults and mark stateful methods
	m := &MethodsConfig{
		PreserveDefaultMethods: true,
		Definitions: map[string]*CacheMethodConfig{
			"custom_method": {
				Finalized: true,
			},
		},
	}

	err := m.SetDefaults()
	assert.NoError(t, err, "SetDefaults should not fail")

	// Verify that default stateful methods are marked as stateful
	for _, methodName := range DefaultStatefulMethodNames {
		method, exists := m.Definitions[methodName]
		assert.True(t, exists, "Default stateful method %s should exist in definitions", methodName)
		if exists {
			assert.True(t, method.Stateful, "Default stateful method %s should be marked as stateful", methodName)
		}
	}

	// Verify some default cache methods exist (since PreserveDefaultMethods=true)
	_, exists := m.Definitions["eth_chainId"]
	assert.True(t, exists, "Default cache method 'eth_chainId' should exist when PreserveDefaultMethods=true")

	// Verify custom method still exists
	_, exists = m.Definitions["custom_method"]
	assert.True(t, exists, "Custom method 'custom_method' should still exist in definitions")
}

func TestMethodsConfigStatefulMethodsNoCustomDefinitions(t *testing.T) {
	// Test case 3: No custom definitions provided
	// Should use all defaults including stateful methods
	m := &MethodsConfig{}

	err := m.SetDefaults()
	assert.NoError(t, err, "SetDefaults should not fail")

	// Verify that default stateful methods are marked as stateful
	for _, methodName := range DefaultStatefulMethodNames {
		method, exists := m.Definitions[methodName]
		assert.True(t, exists, "Default stateful method %s should exist in definitions", methodName)
		if exists {
			assert.True(t, method.Stateful, "Default stateful method %s should be marked as stateful", methodName)
		}
	}

	// Verify some default cache methods exist
	_, exists := m.Definitions["eth_chainId"]
	assert.True(t, exists, "Default cache method 'eth_chainId' should exist")
}

func TestMethodsConfigStatefulMethodOverride(t *testing.T) {
	// Test case 4: User tries to override a default stateful method
	// The stateful flag should still be enforced
	m := &MethodsConfig{
		PreserveDefaultMethods: false,
		Definitions: map[string]*CacheMethodConfig{
			"eth_newFilter": {
				Finalized: true,
				Stateful:  false, // User tries to make it non-stateful
			},
		},
	}

	err := m.SetDefaults()
	assert.NoError(t, err, "SetDefaults should not fail")

	// Verify that eth_newFilter is still marked as stateful
	method, exists := m.Definitions["eth_newFilter"]
	assert.True(t, exists, "Method 'eth_newFilter' should exist in definitions")
	if exists {
		assert.True(t, method.Stateful, "Method 'eth_newFilter' should be marked as stateful even when user tries to override")
	}
}

func TestSetDefaults_NetworkConfig_FailsafeMatchMethod(t *testing.T) {
	// This test suite covers the fix for the bug where user-defined matchMethod
	// patterns were being incorrectly overwritten when no matching default was found.
	// The fix ensures that when no matching default exists, a base default with
	// MatchMethod="*" is used, preserving the user's specific matchMethod.

	t.Run("UserFailsafeWithSpecificMethodNotOverwrittenByUnmatchedDefault", func(t *testing.T) {
		// User defines failsafe for eth_getLogs|eth_getBlockReceipts
		// Defaults define a different pattern (eth_call)
		// User's matchMethod should NOT be overwritten
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(5 * time.Second),
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// Critical: User's matchMethod should be preserved
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", network.Failsafe[0].MatchMethod)
		// User's timeout should be preserved
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
	})

	t.Run("UserFailsafeWithMultipleSpecificMethodsPreserved", func(t *testing.T) {
		// Scenario similar to the erpc.yaml example:
		// User has multiple failsafe configs with specific matchMethod and matchFinality
		// Defaults don't match any of them - user's matchMethod should be preserved
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs|eth_getBlockReceipts",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
				{
					MatchFinality: []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnfinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(6 * time.Second),
					},
				},
				{
					MatchMethod:   "eth_getLogs|eth_getBlockReceipts",
					MatchFinality: []DataFinalityState{DataFinalityStateUnknown},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
				{
					MatchFinality: []DataFinalityState{DataFinalityStateFinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(20 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_sendTransaction",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(30 * time.Second),
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 4)

		// All user matchMethod values should be preserved
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "*", network.Failsafe[1].MatchMethod) // Empty becomes "*"
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", network.Failsafe[2].MatchMethod)
		assert.Equal(t, "*", network.Failsafe[3].MatchMethod) // Empty becomes "*"

		// User timeouts should be preserved
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		assert.Equal(t, "6s", network.Failsafe[1].Timeout.Duration.Resolve(nil).String())
		assert.Equal(t, "10s", network.Failsafe[2].Timeout.Duration.Resolve(nil).String())
		assert.Equal(t, "20s", network.Failsafe[3].Timeout.Duration.Resolve(nil).String())
	})

	t.Run("UserFailsafeMatchesDefaultByMethodAndFinality", func(t *testing.T) {
		// User defines failsafe that matches a default by both method and finality
		// Default values should be merged
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Default retry should be applied since user didn't define it
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("UserFailsafeNoMethodDefaultHasMethod_NoMatch", func(t *testing.T) {
		// User has no matchMethod, default has matchMethod
		// They should NOT match (only one has method specified)
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					// No MatchMethod specified
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// matchMethod should become "*" (default)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Retry should NOT be applied (no match)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("UserFailsafeHasMethodDefaultNoMethod_NoMatch", func(t *testing.T) {
		// User has matchMethod, default has no matchMethod
		// They should NOT match (only one has method specified)
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					// No MatchMethod specified
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// User's matchMethod should be preserved
		assert.Equal(t, "eth_call", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Retry should NOT be applied (no match)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("BothNoMethod_ShouldMatch", func(t *testing.T) {
		// Both user and default have no matchMethod
		// They should match
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					// No MatchMethod specified
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					// No MatchMethod specified
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// matchMethod should become "*" (default, inherited from matching default)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", network.Failsafe[0].Timeout.Duration.Resolve(nil).String())
		// Retry SHOULD be applied (they match)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("FinalityMatchOnly_EmptyFinalityMatchesAny", func(t *testing.T) {
		// Both have no matchMethod, user has finality, default has empty finality
		// Empty finality should match any finality
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchFinality: []DataFinalityState{DataFinalityStateFinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					// Empty MatchFinality matches any
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// Should have matched (empty finality matches any)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("FinalityMismatch_ShouldNotMatch", func(t *testing.T) {
		// Both have no matchMethod, but finalities don't overlap
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchFinality: []DataFinalityState{DataFinalityStateFinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchFinality: []DataFinalityState{DataFinalityStateRealtime},
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// Should NOT have matched (finalities don't overlap)
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("WildcardMethodMatch", func(t *testing.T) {
		// Default has wildcard pattern that matches user's method
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_get*",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// User's specific matchMethod should be preserved
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		// Retry SHOULD be applied (wildcard matches)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("PipePatternDoesNotMatchLiteralPipeValue", func(t *testing.T) {
		// User has pipe pattern "eth_getLogs|eth_getBlockReceipts" as value
		// Default has same pipe pattern "eth_getLogs|eth_getBlockReceipts" as pattern
		// WildcardMatch treats | as OR, so the value "eth_getLogs|eth_getBlockReceipts"
		// doesn't match either "eth_getLogs" or "eth_getBlockReceipts" (the OR branches)
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", network.Failsafe[0].MatchMethod)
		// Retry should NOT be applied - WildcardMatch parses | as OR,
		// so literal "eth_getLogs|eth_getBlockReceipts" doesn't match the OR branches
		assert.Nil(t, network.Failsafe[0].Retry)
	})

	t.Run("WildcardStarMatchesPipeValue", func(t *testing.T) {
		// Default has wildcard "*" pattern which should match any value including pipe patterns
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "*",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// User's matchMethod should be preserved
		assert.Equal(t, "eth_getLogs|eth_getBlockReceipts", network.Failsafe[0].MatchMethod)
		// Retry SHOULD be applied (* matches anything)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("MultipleDefaultsFirstMatchWins", func(t *testing.T) {
		// Multiple defaults, first matching one should be used
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 3,
					},
				},
				{
					MatchMethod: "eth_get*",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 10,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// First match (exact) should be used, not the wildcard
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 3, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("NoUserFailsafe_DefaultsCopied", func(t *testing.T) {
		// No user failsafe defined, defaults should be copied entirely
		network := &NetworkConfig{}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(5 * time.Second),
					},
				},
				{
					MatchMethod: "eth_call",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 2)
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_call", network.Failsafe[1].MatchMethod)
	})

	t.Run("EmptyDefaultsFailsafe_UserConfigPreserved", func(t *testing.T) {
		// Defaults have empty Failsafe array, user config should get system defaults
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		assert.NotNil(t, network.Failsafe[0].Retry)
		// System defaults for retry should be applied
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
		assert.NotZero(t, network.Failsafe[0].Retry.BackoffFactor) // System default
	})

	t.Run("NoDefaults_UserConfigPreserved", func(t *testing.T) {
		// No defaults provided, user config should get system defaults
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, nil)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("PipePatternInDefaultMatchesSingleMethodValue", func(t *testing.T) {
		// User has "eth_getLogs", default has "eth_getLogs|eth_getBlockReceipts"
		// WildcardMatch parses | as OR, so "eth_getLogs" DOES match "eth_getLogs|eth_getBlockReceipts"
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// User's matchMethod should be preserved
		assert.Equal(t, "eth_getLogs", network.Failsafe[0].MatchMethod)
		// Retry SHOULD be applied (eth_getLogs matches the OR pattern)
		assert.NotNil(t, network.Failsafe[0].Retry)
		assert.EqualValues(t, 5, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("UnrelatedMethodDoesNotMatch", func(t *testing.T) {
		// User has "eth_call", default has "eth_getLogs|eth_getBlockReceipts"
		// These should NOT match
		network := &NetworkConfig{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Timeout: &TimeoutPolicyConfig{
						Duration: NewStaticDuration(10 * time.Second),
					},
				},
			},
		}

		defaults := &NetworkDefaults{
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_getLogs|eth_getBlockReceipts",
					Retry: &RetryPolicyConfig{
						MaxAttempts: 5,
					},
				},
			},
		}

		err := network.SetDefaults(nil, defaults)
		assert.NoError(t, err)
		assert.Len(t, network.Failsafe, 1)
		// User's matchMethod should be preserved
		assert.Equal(t, "eth_call", network.Failsafe[0].MatchMethod)
		// Retry should NOT be applied (eth_call doesn't match eth_getLogs|eth_getBlockReceipts)
		assert.Nil(t, network.Failsafe[0].Retry)
	})
}

func TestBuildProviderSettings(t *testing.T) {
	// Goldsky shorthand: authority is the Edge secret token.
	t.Run("goldsky with secret in authority", func(t *testing.T) {
		endpoint, _ := url.Parse("goldsky://my-edge-secret")
		settings, err := buildProviderSettings("goldsky", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "my-edge-secret", settings["secret"])
		assert.Nil(t, settings["tier"])
	})

	t.Run("goldsky with tier query param", func(t *testing.T) {
		endpoint, _ := url.Parse("goldsky://my-edge-secret?tier=custom")
		settings, err := buildProviderSettings("goldsky", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "my-edge-secret", settings["secret"])
		assert.Equal(t, "custom", settings["tier"])
	})

	t.Run("goldsky with secret query param fallback", func(t *testing.T) {
		endpoint, _ := url.Parse("goldsky://?secret=query-secret")
		settings, err := buildProviderSettings("goldsky", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "query-secret", settings["secret"])
	})

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

	// Test case for QuickNode with tag filters
	t.Run("quicknode with filters", func(t *testing.T) {
		endpoint, _ := url.Parse("quicknode://test-api-key?tagIds=123,456&tagLabels=production,staging")
		settings, err := buildProviderSettings("quicknode", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", settings["apiKey"])
		assert.Equal(t, []int{123, 456}, settings["tagIds"])
		assert.Equal(t, []string{"production", "staging"}, settings["tagLabels"])
	})

	t.Run("quicknode without filters", func(t *testing.T) {
		endpoint, _ := url.Parse("quicknode://test-api-key")
		settings, err := buildProviderSettings("quicknode", endpoint)
		assert.NoError(t, err)
		assert.Equal(t, "test-api-key", settings["apiKey"])
		assert.Nil(t, settings["tagIds"])
		assert.Nil(t, settings["tagLabels"])
	})
}

func TestSetDefaults_ConsensusWaitCaps(t *testing.T) {
	t.Run("populates adaptive defaults when unset", func(t *testing.T) {
		c := &ConsensusPolicyConfig{MaxParticipants: 3, AgreementThreshold: 2}
		require := assert.New(t)

		err := c.SetDefaults()
		require.NoError(err)

		require.NotNil(c.MaxWaitOnResult)
		assert.Equal(t, 0.5, c.MaxWaitOnResult.Quantile)
		assert.Equal(t, Duration(5*time.Millisecond), c.MaxWaitOnResult.Min)
		assert.Equal(t, Duration(1*time.Second), c.MaxWaitOnResult.Max)

		require.NotNil(c.MaxWaitOnEmpty)
		assert.Equal(t, 0.9, c.MaxWaitOnEmpty.Quantile)
		assert.Equal(t, Duration(50*time.Millisecond), c.MaxWaitOnEmpty.Min)
		assert.Equal(t, Duration(2*time.Second), c.MaxWaitOnEmpty.Max)
	})

	t.Run("preserves user values", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			MaxWaitOnResult:    NewStaticDuration(250 * time.Millisecond),
			MaxWaitOnEmpty:     NewStaticDuration(800 * time.Millisecond),
		}
		require := assert.New(t)
		require.NoError(c.SetDefaults())
		assert.Equal(t, Duration(250*time.Millisecond), c.MaxWaitOnResult.Base)
		assert.Equal(t, float64(0), c.MaxWaitOnResult.Quantile)
		assert.Equal(t, Duration(800*time.Millisecond), c.MaxWaitOnEmpty.Base)
	})
}

// captureWarnings rebinds the package-level zerolog `log.Logger` to a
// JSON-encoded buffer for the duration of `fn`, then restores the
// prior logger. Used to assert that `SetDefaults` emits a deprecation
// warning when the operator wrote the legacy `evalPerMethod` /
// `evalPerFinality` bools.
//
// `util.ConfigureTestLogger` (init_test.go) sets the global level to
// Disabled when `LOG_LEVEL` is unset — which suppresses every log
// regardless of which logger is configured. We temporarily lift the
// global level to Warn so our capture sees the warning, then restore.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	buf := &bytes.Buffer{}
	prevLogger := log.Logger
	prevLevel := zerolog.GlobalLevel()
	log.Logger = zerolog.New(buf)
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	defer func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	}()
	fn()
	return buf.String()
}

// TestSetDefaults_SelectionPolicy_EvalScope covers the config-load-
// time translation from the `evalPerMethod` / `evalPerFinality` alias
// bools to the canonical `evalScope` enum. Three invariants:
//
//  1. Alias bools alone (no explicit `evalScope`) map to the matching
//     enum value.
//  2. SetDefaults nils out the alias fields after translation —
//     downstream code MUST NOT see stale values.
//  3. Translation is SILENT — no warnings, no log noise. The aliases
//     are a config-shape convenience for backward compat on configs
//     from main; we don't browbeat operators about using them.
func TestSetDefaults_SelectionPolicy_EvalScope(t *testing.T) {
	t.Run("default — nothing set", func(t *testing.T) {
		c := &SelectionPolicyConfig{}
		require.NoError(t, c.SetDefaults())
		assert.Equal(t, EvalScopeNetwork, c.EvalScope)
		assert.Nil(t, c.EvalPerMethod)
		assert.Nil(t, c.EvalPerFinality)
	})

	t.Run("alias bools translate + nil out + stay silent", func(t *testing.T) {
		for name, tc := range map[string]struct {
			perMethod, perFinality *bool
			wantScope              EvalScope
		}{
			"perMethod only":      {boolPtr(true), nil, EvalScopeNetworkMethod},
			"perFinality only":    {nil, boolPtr(true), EvalScopeNetworkFinality},
			"both true":           {boolPtr(true), boolPtr(true), EvalScopeNetworkMethodFinality},
			"perMethod=false":     {boolPtr(false), nil, EvalScopeNetwork},
			"both explicit false": {boolPtr(false), boolPtr(false), EvalScopeNetwork},
		} {
			t.Run(name, func(t *testing.T) {
				c := &SelectionPolicyConfig{
					EvalPerMethod:   tc.perMethod,
					EvalPerFinality: tc.perFinality,
				}
				warnings := captureWarnings(t, func() {
					require.NoError(t, c.SetDefaults())
				})
				assert.Equal(t, tc.wantScope, c.EvalScope,
					"resolved EvalScope after translation")
				assert.Nil(t, c.EvalPerMethod, "alias field niled after translation")
				assert.Nil(t, c.EvalPerFinality, "alias field niled after translation")
				assert.Empty(t, warnings,
					"alias-bool translation is silent — no deprecation noise")
			})
		}
	})

	t.Run("explicit evalScope wins silently over alias bools", func(t *testing.T) {
		c := &SelectionPolicyConfig{
			EvalScope:       EvalScopeNetworkFinality, // explicit
			EvalPerMethod:   boolPtr(true),            // alias — ignored
			EvalPerFinality: boolPtr(false),
		}
		warnings := captureWarnings(t, func() {
			require.NoError(t, c.SetDefaults())
		})
		assert.Equal(t, EvalScopeNetworkFinality, c.EvalScope,
			"explicit evalScope wins")
		assert.Nil(t, c.EvalPerMethod, "alias field niled after override")
		assert.Nil(t, c.EvalPerFinality, "alias field niled after override")
		assert.Empty(t, warnings,
			"silent translation — no warning even when both are set")
	})

	t.Run("no warning when only modern evalScope is set", func(t *testing.T) {
		c := &SelectionPolicyConfig{EvalScope: EvalScopeNetworkMethod}
		warnings := captureWarnings(t, func() {
			require.NoError(t, c.SetDefaults())
		})
		assert.Empty(t, warnings,
			"no alias fields touched → no log noise")
	})

	t.Run("invalid evalScope rejects", func(t *testing.T) {
		c := &SelectionPolicyConfig{EvalScope: "bogus"}
		err := c.SetDefaults()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "evalScope")
	})
}

// TestRedisIAMAuthDefaults verifies that SetDefaults lowercases the cacheName
// and auto-enables TLS so the URI uses rediss://.
func TestRedisIAMAuthDefaults(t *testing.T) {
	t.Parallel()

	cfg := &RedisConnectorConfig{
		Addr: "MY-CLUSTER.example.com:6379",
		IAMAuth: &RedisIAMAuthConfig{
			Enabled:   true,
			CacheName: "MY-CLUSTER",
			Region:    "us-east-1",
			UserID:    "iam-user-01",
		},
	}
	err := cfg.SetDefaults()
	require.NoError(t, err)

	assert.Equal(t, "my-cluster", cfg.IAMAuth.CacheName, "SetDefaults must lowercase cacheName")
	assert.NotNil(t, cfg.TLS, "SetDefaults must create TLS config when IAM auth is on")
	assert.True(t, cfg.TLS.Enabled, "TLS must be enabled when IAM auth is on")
	assert.True(t, strings.HasPrefix(cfg.URI, "rediss://"), "URI must use rediss:// when IAM auth is on, got: %s", cfg.URI)
}

// TestPostgreSQLIAMAuthDeriveFromURI verifies that SetDefaults derives Endpoint
// and DBUser from ConnectionUri and appends sslmode=require automatically.
func TestPostgreSQLIAMAuthDeriveFromURI(t *testing.T) {
	t.Parallel()

	cfg := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://erpc-user@mydb.abc123.us-east-1.rds.amazonaws.com:5432/erpc",
		IAMAuth: &PostgreSQLIAMAuthConfig{
			Enabled: true,
			Region:  "us-east-1",
			// Endpoint and DBUser intentionally left empty — should be derived.
		},
	}

	err := cfg.SetDefaults(connectorScopeCache)
	require.NoError(t, err)

	assert.Equal(t, "mydb.abc123.us-east-1.rds.amazonaws.com:5432", cfg.IAMAuth.Endpoint, "Endpoint must be derived from ConnectionUri")
	assert.Equal(t, "erpc-user", cfg.IAMAuth.DBUser, "DBUser must be derived from ConnectionUri")
	assert.Contains(t, cfg.ConnectionUri, "sslmode=require", "sslmode=require must be appended automatically")
}

// TestPostgreSQLIAMAuthDeriveFromURIExplicitOverrides verifies that explicitly
// set Endpoint/DBUser are not overwritten by SetDefaults.
func TestPostgreSQLIAMAuthDeriveFromURIExplicitOverrides(t *testing.T) {
	t.Parallel()

	cfg := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://erpc-user@mydb.abc123.us-east-1.rds.amazonaws.com:5432/erpc",
		IAMAuth: &PostgreSQLIAMAuthConfig{
			Enabled:  true,
			Region:   "us-east-1",
			Endpoint: "custom-endpoint:5432",
			DBUser:   "custom-user",
		},
	}

	err := cfg.SetDefaults(connectorScopeCache)
	require.NoError(t, err)

	assert.Equal(t, "custom-endpoint:5432", cfg.IAMAuth.Endpoint, "explicit Endpoint must not be overwritten")
	assert.Equal(t, "custom-user", cfg.IAMAuth.DBUser, "explicit DBUser must not be overwritten")
}
