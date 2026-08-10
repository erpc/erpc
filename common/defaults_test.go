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

// TestSetDefaults_ConsensusIgnoreFields locks three contracts of the
// IgnoreFields defaulting added for SVM consensus:
//
//  1. EVM invariance — the eth_* entries are byte-for-byte what they were
//     before the SVM change (a regression guard, not a defaults echo).
//  2. SVM context-envelope methods each ignore exactly
//     ["context.slot","context.apiVersion"].
//  3. Operator-supplied IgnoreFields wins wholesale: SetDefaults must not
//     inject SVM entries into a non-nil map (nil-check semantics).
func TestSetDefaults_ConsensusIgnoreFields(t *testing.T) {
	t.Run("fresh config gets EVM entries unchanged and SVM envelope entries", func(t *testing.T) {
		c := &ConsensusPolicyConfig{MaxParticipants: 3, AgreementThreshold: 2}
		require := assert.New(t)
		require.NoError(c.SetDefaults())
		require.NotNil(c.IgnoreFields)

		// EVM invariance: exactly the pre-SVM values.
		assert.Equal(t, []string{"*.blockTimestamp"}, c.IgnoreFields["eth_getLogs"])
		assert.Equal(t, []string{"blockTimestamp", "logs.*.blockTimestamp"}, c.IgnoreFields["eth_getTransactionReceipt"])
		assert.Equal(t, []string{"*.blockTimestamp", "*.logs.*.blockTimestamp"}, c.IgnoreFields["eth_getBlockReceipts"])

		// SVM RpcResponse-enveloped methods ignore the context envelope only.
		for _, m := range []string{
			"getAccountInfo",
			"getBalance",
			"getLatestBlockhash",
			"getMultipleAccounts",
			"getSignatureStatuses",
			"getTokenAccountsByOwner",
			"simulateTransaction",
		} {
			assert.Equal(t, []string{"context.slot", "context.apiVersion"}, c.IgnoreFields[m], "method %s", m)
		}

		// Scalar / non-enveloped Solana methods are deliberately absent:
		// their whole result is the payload, so nothing may be ignored.
		assert.NotContains(t, c.IgnoreFields, "getEpochInfo")
		assert.NotContains(t, c.IgnoreFields, "getSlot")
	})

	t.Run("operator-supplied IgnoreFields is left exactly as given", func(t *testing.T) {
		c := &ConsensusPolicyConfig{
			MaxParticipants:    3,
			AgreementThreshold: 2,
			IgnoreFields:       map[string][]string{"foo": {"bar"}},
		}
		require := assert.New(t)
		require.NoError(c.SetDefaults())
		assert.Equal(t, map[string][]string{"foo": {"bar"}}, c.IgnoreFields)
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

// int64Ptr builds a *int64 for SvmNetworkConfig.MaxFinalizedSlotLag, where nil
// ("unset") and 0 ("lag filter disabled") mean different things.
//
// ponytail: Go 1.26's `new(int64(0))` would replace this helper, but go.mod still
// declares `go 1.25.1` and the compiler rejects new(expr) below 1.26. Delete this
// and inline new(...) when the go directive is bumped.
func int64Ptr(v int64) *int64 { return &v }

func TestSetDefaults_SvmNetworkConfig_PopulatesGuards(t *testing.T) {
	t.Run("zero values get defaults", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta"},
		}
		require.NoError(t, n.SetDefaults(nil, nil))

		require.NotNil(t, n.Svm.MaxFinalizedSlotLag, "SetDefaults must materialize the lag default")
		if *n.Svm.MaxFinalizedSlotLag != MaxShredInsertSlotLagThreshold {
			t.Errorf("MaxFinalizedSlotLag = %d, want %d", *n.Svm.MaxFinalizedSlotLag, MaxShredInsertSlotLagThreshold)
		}
		if n.Svm.StatePollerDebounce.Duration() != 400*time.Millisecond {
			t.Errorf("StatePollerDebounce = %v, want 400ms", n.Svm.StatePollerDebounce.Duration())
		}
	})

	t.Run("operator overrides preserved", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm: &SvmNetworkConfig{
				Cluster:             "mainnet-beta",
				MaxFinalizedSlotLag: int64Ptr(5000),
				StatePollerDebounce: Duration(750 * time.Millisecond),
			},
		}
		require.NoError(t, n.SetDefaults(nil, nil))

		require.NotNil(t, n.Svm.MaxFinalizedSlotLag)
		if *n.Svm.MaxFinalizedSlotLag != 5000 {
			t.Errorf("operator value should win, got %d", *n.Svm.MaxFinalizedSlotLag)
		}
		if n.Svm.StatePollerDebounce.Duration() != 750*time.Millisecond {
			t.Errorf("operator debounce should win, got %v", n.Svm.StatePollerDebounce.Duration())
		}
	})

	// The documented contract: 0 disables the lag filter. A non-pointer int64
	// made this unreachable — SetDefaults could not tell it from "unset" and
	// overwrote it with 100.
	t.Run("explicit zero disables the lag filter and survives SetDefaults", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta", MaxFinalizedSlotLag: int64Ptr(0)},
		}
		require.NoError(t, n.SetDefaults(nil, nil))

		require.NotNil(t, n.Svm.MaxFinalizedSlotLag)
		require.Equal(t, int64(0), *n.Svm.MaxFinalizedSlotLag,
			"an explicit 0 must survive SetDefaults so readers see the filter as disabled")
	})

	t.Run("Architecture auto-derived from Svm", func(t *testing.T) {
		n := &NetworkConfig{Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}}
		require.NoError(t, n.SetDefaults(nil, nil))

		if n.Architecture != ArchitectureSvm {
			t.Errorf("expected Architecture=svm, got %q", n.Architecture)
		}
	})

	t.Run("Architecture=svm without Svm section auto-creates it", func(t *testing.T) {
		n := &NetworkConfig{Architecture: ArchitectureSvm}
		require.NoError(t, n.SetDefaults(nil, nil))

		if n.Svm == nil {
			t.Fatal("Svm should be auto-created when Architecture=svm")
		}
		require.NotNil(t, n.Svm.MaxFinalizedSlotLag, "defaults should still apply to auto-created Svm")
		require.Equal(t, MaxShredInsertSlotLagThreshold, *n.Svm.MaxFinalizedSlotLag)
	})
}

func TestSetDefaults_NetworkDefaults_SvmMergesIntoNetwork(t *testing.T) {
	defaults := &NetworkDefaults{
		Svm: &SvmNetworkConfig{
			Commitment:          "confirmed",
			StatePollerDebounce: Duration(500 * time.Millisecond),
			Cluster:             "devnet", // must be ignored when merging into network
		},
	}

	t.Run("inherits commitment and debounce from networkDefaults", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta"},
		}
		require.NoError(t, n.SetDefaults(nil, defaults))

		require.Equal(t, "mainnet-beta", n.Svm.Cluster)
		require.Equal(t, "confirmed", n.Svm.Commitment)
		require.Equal(t, 500*time.Millisecond, n.Svm.StatePollerDebounce.Duration())
	})

	t.Run("network override wins over networkDefaults", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm: &SvmNetworkConfig{
				Cluster:    "mainnet-beta",
				Commitment: "finalized",
			},
		}
		require.NoError(t, n.SetDefaults(nil, defaults))

		require.Equal(t, "finalized", n.Svm.Commitment)
	})

	t.Run("cluster never copied from networkDefaults", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta"},
		}
		require.NoError(t, n.SetDefaults(nil, defaults))

		require.Equal(t, "mainnet-beta", n.Svm.Cluster)
	})

	t.Run("does not auto-create Svm from networkDefaults on non-SVM network", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureEvm,
			Evm:          &EvmNetworkConfig{ChainId: 1},
		}
		require.NoError(t, n.SetDefaults(nil, defaults))

		require.Nil(t, n.Svm)
	})

	// Both of these are disable switches an operator can only express as a
	// falsy value, so a zero-test in the merge silently discarded them and the
	// guard stayed on with no way to turn it off.
	t.Run("networkDefaults enforceBlockAvailability=false survives the merge", func(t *testing.T) {
		d := &NetworkDefaults{Svm: &SvmNetworkConfig{EnforceBlockAvailability: util.BoolPtr(false)}}
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta"},
		}
		require.NoError(t, n.SetDefaults(nil, d))

		require.NotNil(t, n.Svm.EnforceBlockAvailability, "an explicit false must not be dropped by the merge")
		require.False(t, *n.Svm.EnforceBlockAvailability)
	})

	t.Run("networkDefaults maxFinalizedSlotLag=0 survives the merge and disables the filter", func(t *testing.T) {
		d := &NetworkDefaults{Svm: &SvmNetworkConfig{MaxFinalizedSlotLag: int64Ptr(0)}}
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta"},
		}
		require.NoError(t, n.SetDefaults(nil, d))

		require.NotNil(t, n.Svm.MaxFinalizedSlotLag)
		require.Equal(t, int64(0), *n.Svm.MaxFinalizedSlotLag,
			"networkDefaults 0 must reach the network, not be overwritten by SetDefaults' 100")
	})

	t.Run("network-level pointer overrides win over networkDefaults", func(t *testing.T) {
		d := &NetworkDefaults{Svm: &SvmNetworkConfig{
			EnforceBlockAvailability: util.BoolPtr(false),
			MaxFinalizedSlotLag:      int64Ptr(0),
		}}
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm: &SvmNetworkConfig{
				Cluster:                  "mainnet-beta",
				EnforceBlockAvailability: util.BoolPtr(true),
				MaxFinalizedSlotLag:      int64Ptr(42),
			},
		}
		require.NoError(t, n.SetDefaults(nil, d))

		require.True(t, *n.Svm.EnforceBlockAvailability)
		require.Equal(t, int64(42), *n.Svm.MaxFinalizedSlotLag)
	})

	t.Run("inherited pointers are copied, not shared between networks", func(t *testing.T) {
		d := &NetworkDefaults{Svm: &SvmNetworkConfig{MaxFinalizedSlotLag: int64Ptr(7)}}
		a := &NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}}
		b := &NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Cluster: "devnet"}}
		require.NoError(t, a.SetDefaults(nil, d))
		require.NoError(t, b.SetDefaults(nil, d))

		require.NotSame(t, a.Svm.MaxFinalizedSlotLag, b.Svm.MaxFinalizedSlotLag,
			"two networks must not alias one operator-supplied pointer")
		require.Equal(t, int64(7), *d.Svm.MaxFinalizedSlotLag, "the defaults block itself must stay unmutated")
	})
}

// TestSetDefaults_SvmNetworkNotPollutedByEvmDefaults regression-locks the fix
// where networkDefaults.evm was copied into SVM networks. Because architecture
// derivation checks n.Evm before n.Svm, an `svm:`-authored network without an
// explicit architecture silently became architecture=evm.
func TestSetDefaults_SvmNetworkNotPollutedByEvmDefaults(t *testing.T) {
	newDefaults := func() *NetworkDefaults {
		return &NetworkDefaults{
			Evm: &EvmNetworkConfig{GetLogsMaxAllowedRange: 30000},
		}
	}

	t.Run("svm network with architecture unset stays svm and gets no evm block", func(t *testing.T) {
		n := &NetworkConfig{Svm: &SvmNetworkConfig{Cluster: "mainnet-beta"}}
		require.NoError(t, n.SetDefaults(nil, newDefaults()))

		assert.Nil(t, n.Evm, "networkDefaults.evm must not be injected into an svm network")
		assert.Equal(t, ArchitectureSvm, n.Architecture, "architecture must derive to svm, not evm")
	})

	t.Run("explicit architecture=svm stays svm and gets no evm block", func(t *testing.T) {
		n := &NetworkConfig{Architecture: ArchitectureSvm}
		require.NoError(t, n.SetDefaults(nil, newDefaults()))

		assert.Nil(t, n.Evm, "networkDefaults.evm must not be injected when architecture=svm")
		assert.Equal(t, ArchitectureSvm, n.Architecture)
	})

	t.Run("evm invariance: network with neither evm nor svm still receives evm defaults", func(t *testing.T) {
		n := &NetworkConfig{}
		require.NoError(t, n.SetDefaults(nil, newDefaults()))

		require.NotNil(t, n.Evm, "evm defaults must still be copied onto plain networks")
		assert.Equal(t, ArchitectureEvm, n.Architecture, "architecture must derive to evm")
		assert.EqualValues(t, 30000, n.Evm.GetLogsMaxAllowedRange, "copied from networkDefaults.evm")
	})

	t.Run("evm invariance: network with own evm block still field-merges from defaults", func(t *testing.T) {
		n := &NetworkConfig{Evm: &EvmNetworkConfig{ChainId: 1}}
		require.NoError(t, n.SetDefaults(nil, newDefaults()))

		assert.EqualValues(t, 30000, n.Evm.GetLogsMaxAllowedRange, "zero field inherited from networkDefaults.evm")
		assert.EqualValues(t, 1, n.Evm.ChainId, "operator value preserved")
	})
}

func TestDatabaseConfig_SetDefaults_SvmJsonRpcCache(t *testing.T) {
	d := &DatabaseConfig{
		SvmJsonRpcCache: &CacheConfig{
			Connectors: []*ConnectorConfig{
				{
					Id:     "short-term",
					Driver: DriverMemory,
					Memory: &MemoryConnectorConfig{},
				},
			},
		},
	}
	require.NoError(t, d.SetDefaults("erpc-default"))
	require.Equal(t, "1GB", d.SvmJsonRpcCache.Connectors[0].Memory.MaxTotalSize)
	require.Equal(t, 100_000, d.SvmJsonRpcCache.Connectors[0].Memory.MaxItems)
}

// TestApplyDefaults_UpstreamDefaultsSvm locks in the wiring of
// upstreamDefaults.svm, which was previously not merged into per-upstream SVM
// config at all — operators had to repeat chain/cluster on every upstream.
func TestApplyDefaults_UpstreamDefaultsSvm(t *testing.T) {
	newDefaults := func() *UpstreamConfig {
		return &UpstreamConfig{Svm: &SvmUpstreamConfig{
			Chain:            "fogo",
			Cluster:          "mainnet",
			CheckGenesisHash: true,
		}}
	}

	t.Run("upstream without an svm block inherits the whole template", func(t *testing.T) {
		u := &UpstreamConfig{Id: "u1", Type: UpstreamTypeSvm, Endpoint: "http://localhost:8899"}
		require.NoError(t, u.ApplyDefaults(newDefaults()))

		require.NotNil(t, u.Svm, "upstreamDefaults.svm must reach the upstream")
		require.Equal(t, "fogo", u.Svm.Chain)
		require.Equal(t, "mainnet", u.Svm.Cluster)
		require.True(t, u.Svm.CheckGenesisHash)
	})

	t.Run("upstream's own values win, empty fields inherit", func(t *testing.T) {
		u := &UpstreamConfig{
			Id: "u1", Type: UpstreamTypeSvm, Endpoint: "http://localhost:8899",
			Svm: &SvmUpstreamConfig{Cluster: "testnet"},
		}
		require.NoError(t, u.ApplyDefaults(newDefaults()))

		require.Equal(t, "testnet", u.Svm.Cluster, "explicit cluster must win")
		require.Equal(t, "fogo", u.Svm.Chain, "empty chain must inherit")
		require.True(t, u.Svm.CheckGenesisHash, "opt-in genesis check must propagate")
	})

	t.Run("inherited svm block is copied, not shared", func(t *testing.T) {
		d := newDefaults()
		a := &UpstreamConfig{Id: "a", Type: UpstreamTypeSvm, Endpoint: "http://a:8899"}
		b := &UpstreamConfig{Id: "b", Type: UpstreamTypeSvm, Endpoint: "http://b:8899"}
		require.NoError(t, a.ApplyDefaults(d))
		require.NoError(t, b.ApplyDefaults(d))

		a.Svm.Cluster = "devnet"
		require.Equal(t, "mainnet", b.Svm.Cluster, "upstreams must not alias one another's svm block")
		require.Equal(t, "mainnet", d.Svm.Cluster, "the defaults block itself must stay unmutated")
	})

	t.Run("no upstreamDefaults.svm leaves the upstream untouched", func(t *testing.T) {
		u := &UpstreamConfig{Id: "u1", Type: UpstreamTypeEvm, Endpoint: "http://localhost:8545"}
		require.NoError(t, u.ApplyDefaults(&UpstreamConfig{}))

		require.Nil(t, u.Svm)
	})
}

// Hook dispatch (architecture/evm/hooks.go) routes method names
// case-insensitively, so the per-method config lookup must resolve the same
// way — otherwise a non-canonical casing dispatches into method-specific logic
// but resolves no config (no gating, no caching, no block-ref extraction).
// Canonicalization is lookup-only: the wire method string is forwarded
// upstream verbatim (per JSON-RPC 2.0 the method member is case-sensitive).
func TestFindCacheMethodConfig(t *testing.T) {
	t.Parallel()

	a := &CacheMethodConfig{Finalized: true}
	b := &CacheMethodConfig{Realtime: true}

	t.Run("ExactMatchWins", func(t *testing.T) {
		defs := map[string]*CacheMethodConfig{"eth_call": a, "ETH_CALL": b}
		assert.Same(t, a, FindCacheMethodConfig(defs, "eth_call"))
		assert.Same(t, b, FindCacheMethodConfig(defs, "ETH_CALL"))
	})

	t.Run("NonCanonicalCasingResolves", func(t *testing.T) {
		defs := map[string]*CacheMethodConfig{"eth_getBlockByNumber": a}
		assert.Same(t, a, FindCacheMethodConfig(defs, "ETH_GETBLOCKBYNUMBER"))
		assert.Same(t, a, FindCacheMethodConfig(defs, "eth_getblockbynumber"))
		assert.Same(t, a, FindCacheMethodConfig(defs, "eth_GetBlockByNumber"))
	})

	t.Run("OperatorCasedKeyFoundByAnyCasing", func(t *testing.T) {
		defs := map[string]*CacheMethodConfig{"custom_FooBar": a}
		assert.Same(t, a, FindCacheMethodConfig(defs, "CUSTOM_foobar"))
	})

	t.Run("MissReturnsNil", func(t *testing.T) {
		assert.Nil(t, FindCacheMethodConfig(nil, "eth_call"))
		assert.Nil(t, FindCacheMethodConfig(map[string]*CacheMethodConfig{}, "eth_call"))
		assert.Nil(t, FindCacheMethodConfig(map[string]*CacheMethodConfig{"eth_call": a}, "eth_getLogs"))
	})

	t.Run("NilValuedEntriesAreAbsent", func(t *testing.T) {
		defs := map[string]*CacheMethodConfig{"eth_call": nil}
		assert.Nil(t, FindCacheMethodConfig(defs, "eth_call"))
		assert.Nil(t, FindCacheMethodConfig(defs, "ETH_CALL"))
	})

	t.Run("DeterministicAmongMultipleFoldMatches", func(t *testing.T) {
		// Pathological: two non-exact casings of the requested name. The
		// lexicographically smallest key wins, independent of map order.
		defs := map[string]*CacheMethodConfig{"ETH_CALL": b, "eTH_CALL": a}
		for i := 0; i < 50; i++ {
			assert.Same(t, b, FindCacheMethodConfig(defs, "eth_call"))
		}
	})

	t.Run("DefaultTablesResolveNonCanonicalCasing", func(t *testing.T) {
		assert.Same(t,
			DefaultWithBlockCacheMethods["eth_getBlockByNumber"],
			FindCacheMethodConfig(DefaultWithBlockCacheMethods, "ETH_GETBLOCKBYNUMBER"))
		assert.Same(t,
			DefaultStaticCacheMethods["eth_chainId"],
			FindCacheMethodConfig(DefaultStaticCacheMethods, "ETH_CHAINID"))
	})
}

// SetDefaults must not leave two entries that differ only in letter case: the
// merge would otherwise resolve one logical method to different config
// depending on the casing the client happens to send, and the stateful marker
// would land on a phantom canonical entry instead of the operator's own key.
func TestMethodsConfigCaseInsensitiveMerge(t *testing.T) {
	t.Run("DifferentlyCasedOverrideReplacesTheDefault", func(t *testing.T) {
		m := &MethodsConfig{
			PreserveDefaultMethods: true,
			Definitions: map[string]*CacheMethodConfig{
				"Eth_Call": {Finalized: true},
			},
		}
		require.NoError(t, m.SetDefaults())

		_, hasCanonical := m.Definitions["eth_call"]
		assert.False(t, hasCanonical, "the default entry must be replaced, not kept alongside the override")

		// Every casing resolves to the operator's override — not to a default
		// that only canonical-cased traffic would have hit.
		for _, casing := range []string{"eth_call", "Eth_Call", "ETH_CALL"} {
			cfg := m.FindMethodConfig(casing)
			require.NotNil(t, cfg, "casing %s must resolve", casing)
			assert.True(t, cfg.Finalized, "casing %s must resolve to the operator override", casing)
		}
	})

	t.Run("StatefulMarkerLandsOnTheOperatorsOwnKey", func(t *testing.T) {
		m := &MethodsConfig{
			Definitions: map[string]*CacheMethodConfig{
				"Eth_NewFilter": {Finalized: true},
			},
		}
		require.NoError(t, m.SetDefaults())

		_, phantom := m.Definitions["eth_newFilter"]
		assert.False(t, phantom, "no phantom canonical entry beside the operator's key")
		require.NotNil(t, m.Definitions["Eth_NewFilter"])
		assert.True(t, m.Definitions["Eth_NewFilter"].Stateful,
			"the stateful guard must apply to the operator's entry, whatever its casing")
		assert.True(t, m.Definitions["Eth_NewFilter"].Finalized, "the operator's own config survives")

		for _, casing := range []string{"eth_newFilter", "Eth_NewFilter", "ETH_NEWFILTER"} {
			cfg := m.FindMethodConfig(casing)
			require.NotNil(t, cfg, "casing %s must resolve", casing)
			assert.True(t, cfg.Stateful, "casing %s must be gated as stateful", casing)
		}
	})

	t.Run("NullDefinitionForStatefulMethodDoesNotPanic", func(t *testing.T) {
		// YAML `eth_newFilter: ~` unmarshals to a present key with a nil value.
		// It means "no cache config", not "not stateful".
		m := &MethodsConfig{
			Definitions: map[string]*CacheMethodConfig{
				"custom_method": {Finalized: true},
				"eth_newFilter": nil,
			},
		}
		require.NotPanics(t, func() {
			require.NoError(t, m.SetDefaults())
		})
		require.NotNil(t, m.Definitions["eth_newFilter"])
		assert.True(t, m.Definitions["eth_newFilter"].Stateful)
	})

	t.Run("StatefulGuardSurvivesAnOperatorOverride", func(t *testing.T) {
		// Same intent as TestMethodsConfigStatefulMethodOverride, but with
		// defaults preserved — the guard must not depend on which branch ran.
		m := &MethodsConfig{
			PreserveDefaultMethods: true,
			Definitions: map[string]*CacheMethodConfig{
				"eth_newFilter": {Finalized: true, Stateful: false},
			},
		}
		require.NoError(t, m.SetDefaults())
		assert.True(t, m.Definitions["eth_newFilter"].Stateful)
	})
}

// The built-in tables resolve non-canonical casings through their prebuilt
// lowercase index, with the same precedence as the exact-key lookup.
func TestFindDefaultCacheMethodConfig(t *testing.T) {
	t.Parallel()

	assert.Same(t, DefaultWithBlockCacheMethods["eth_getBlockByNumber"],
		FindDefaultCacheMethodConfig("eth_getBlockByNumber"))
	assert.Same(t, DefaultWithBlockCacheMethods["eth_getBlockByNumber"],
		FindDefaultCacheMethodConfig("ETH_GETBLOCKBYNUMBER"))
	assert.Same(t, DefaultStaticCacheMethods["eth_chainId"],
		FindDefaultCacheMethodConfig("eth_chainid"))
	assert.Same(t, DefaultSpecialCacheMethods["eth_getTransactionReceipt"],
		FindDefaultCacheMethodConfig("ETH_GetTransactionReceipt"))
	assert.Nil(t, FindDefaultCacheMethodConfig("custom_unknownMethod"))

	assert.Same(t, DefaultWithBlockCacheMethods["eth_getLogs"],
		DefaultWithBlockMethodConfig("ETH_GETLOGS"))
	assert.Nil(t, DefaultWithBlockMethodConfig("eth_chainId"),
		"the with-block accessor must not reach into the other tables")
}
