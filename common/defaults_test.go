package common

import (
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
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
		assert.NotNil(t, network.Failsafe[0].Matchers)
		assert.Len(t, network.Failsafe[0].Matchers, 1)
		assert.Equal(t, "*", network.Failsafe[0].Matchers[0].Method)
		assert.Equal(t, MatcherInclude, network.Failsafe[0].Matchers[0].Action)
		assert.Equal(t, Duration(100*time.Millisecond), network.Failsafe[0].Timeout.Duration)
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
		assert.Equal(t, Duration(100*time.Millisecond), network.Failsafe[0].Hedge.Delay)
		assert.Equal(t, int(10), network.Failsafe[0].Hedge.MaxCount)
		assert.Equal(t, Duration(100*time.Millisecond), network.Failsafe[0].Hedge.MinDelay)
		assert.Equal(t, Duration(999*time.Second), network.Failsafe[0].Hedge.MaxDelay)
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
		assert.Equal(t, uint(10), network.Failsafe[0].CircuitBreaker.FailureThresholdCount)
		assert.Equal(t, uint(80), network.Failsafe[0].CircuitBreaker.FailureThresholdCapacity)
		assert.Equal(t, Duration(300*time.Second), network.Failsafe[0].CircuitBreaker.HalfOpenAfter)
		assert.Equal(t, uint(8), network.Failsafe[0].CircuitBreaker.SuccessThresholdCount)
		assert.Equal(t, uint(200), network.Failsafe[0].CircuitBreaker.SuccessThresholdCapacity)
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

		expected := &FailsafeConfig{
			MatchMethod: "",
			Matchers: []*MatcherConfig{
				{
					Method: "*",
					Action: MatcherInclude,
				},
			},
			Retry: &RetryPolicyConfig{
				MaxAttempts:       12345,
				Delay:             Duration(0 * time.Millisecond),
				BackoffMaxDelay:   Duration(3 * time.Second),
				BackoffFactor:     1.2,
				Jitter:            Duration(0 * time.Millisecond),
				EmptyResultAccept: DefaultEmptyResultAccept(),
			},
		}
		assert.EqualValues(t, expected, network.Failsafe[0])
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
						Duration: Duration(10 * time.Second),
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
		assert.Equal(t, "10s", upstream.Failsafe[0].Timeout.Duration.String())
		// Retry should NOT be applied (no match)
		assert.Nil(t, upstream.Failsafe[0].Retry)
	})

	t.Run("UpstreamFailsafeUserPoliciesReplaceDefaults", func(t *testing.T) {
		// PR #388 (matchers): when user defines any failsafe policies, those replace
		// defaults entirely — defaults are NOT merged in by method/finality matching.
		// This is a deliberate behavior change from main's per-method merge approach.
		// See PR #388 review comments #11 and #12 for the open design discussion.
		upstream := &UpstreamConfig{
			Endpoint: "http://rpc1.localhost",
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod:   "eth_getLogs",
					MatchFinality: []DataFinalityState{DataFinalityStateUnfinalized},
					Timeout: &TimeoutPolicyConfig{
						Duration: Duration(10 * time.Second),
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
		// User's matchMethod and timeout are preserved
		assert.Equal(t, "eth_getLogs", upstream.Failsafe[0].MatchMethod)
		assert.Equal(t, "10s", upstream.Failsafe[0].Timeout.Duration.String())
		// Retry from defaults is NOT merged in — user policies stand alone
		assert.Nil(t, upstream.Failsafe[0].Retry)
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
			MaxAttempts:       2,
			BackoffMaxDelay:   Duration(10 * time.Second),
			Delay:             Duration(1 * time.Second),
			Jitter:            Duration(500 * time.Millisecond),
			BackoffFactor:     1.2,
			EmptyResultAccept: DefaultEmptyResultAccept(),
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

// TestSetDefaults_FailsafeMatcherInheritance covers PR #388's matchers-aware
// defaulting algorithm. The pre-#388 algorithm merged each user policy with
// any inheritable default policy whose `matchMethod`/`matchFinality` patterns
// overlapped — this is the behavior that 600+ lines of `t.Skip`'d subtests
// documented and that was deliberately removed.
//
// Under the new "user policies replace defaults" semantic, the merge step is
// gone: any user-defined Failsafe entry stands on its own; defaults only fill
// in for entries the user didn't define at all. This change has a real
// production consequence (32 of 509 goldsky upstreams lose method-tier
// failsafe customization when their `*` override is applied), but the team's
// decision is to accept that for the simpler model. These tests pin that
// semantic so future refactors don't drift it.
//
// Replacement for PR #388 review item T-6.
func TestSetDefaults_FailsafeMatcherInheritance(t *testing.T) {
	t.Run("user has no failsafe → all defaults are inherited", func(t *testing.T) {
		ups := &UpstreamConfig{
			Type:     UpstreamTypeEvm,
			Endpoint: "http://test/",
			Failsafe: nil,
		}
		defaults := &UpstreamConfig{
			Failsafe: []*FailsafeConfig{
				{
					Matchers: []*MatcherConfig{{Method: "eth_*", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(2 * time.Second)},
				},
				{
					Matchers: []*MatcherConfig{{Method: "trace_*", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(30 * time.Second)},
				},
			},
		}

		err := ups.SetDefaults(defaults)
		assert.NoError(t, err)
		assert.Len(t, ups.Failsafe, 2, "all defaults should be inherited when user has no failsafe")
		assert.Equal(t, Duration(2*time.Second), ups.Failsafe[0].Timeout.Duration)
		assert.Equal(t, Duration(30*time.Second), ups.Failsafe[1].Timeout.Duration)
	})

	t.Run("user defines partial failsafe → defaults are NOT merged in by matchMethod", func(t *testing.T) {
		// This is the documented production hazard. User's wildcard timeout
		// override entirely replaces the default's nine method-tier timeouts.
		// Pre-#388, the merging algorithm would have kept the method-tier
		// defaults that didn't overlap with the user's matchMethod. Now they
		// are replaced.
		ups := &UpstreamConfig{
			Type:     UpstreamTypeEvm,
			Endpoint: "http://test/",
			Failsafe: []*FailsafeConfig{
				{
					Matchers: []*MatcherConfig{{Method: "*", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(15 * time.Second)},
					Hedge:    &HedgePolicyConfig{Delay: Duration(500 * time.Millisecond), MaxCount: 1},
				},
			},
		}
		defaults := &UpstreamConfig{
			Failsafe: []*FailsafeConfig{
				{
					Matchers: []*MatcherConfig{{Method: "eth_call", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(2 * time.Second)},
				},
				{
					Matchers: []*MatcherConfig{{Method: "trace_*", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(30 * time.Second)},
				},
			},
		}

		err := ups.SetDefaults(defaults)
		assert.NoError(t, err)
		assert.Len(t, ups.Failsafe, 1, "user policy stands alone — defaults not merged")
		assert.Equal(t, Duration(15*time.Second), ups.Failsafe[0].Timeout.Duration,
			"user's timeout override should be preserved")
		assert.NotNil(t, ups.Failsafe[0].Hedge, "user's hedge should be preserved")
	})

	t.Run("user defines full set of policies → defaults play no role", func(t *testing.T) {
		ups := &UpstreamConfig{
			Type:     UpstreamTypeEvm,
			Endpoint: "http://test/",
			Failsafe: []*FailsafeConfig{
				{
					Matchers:       []*MatcherConfig{{Method: "*", Action: MatcherInclude}},
					Retry:          &RetryPolicyConfig{MaxAttempts: 3},
					Timeout:        &TimeoutPolicyConfig{Duration: Duration(5 * time.Second)},
					CircuitBreaker: &CircuitBreakerPolicyConfig{},
				},
			},
		}
		defaults := &UpstreamConfig{
			Failsafe: []*FailsafeConfig{
				{
					Matchers: []*MatcherConfig{{Method: "eth_*", Action: MatcherInclude}},
					Timeout:  &TimeoutPolicyConfig{Duration: Duration(99 * time.Second)},
				},
			},
		}

		err := ups.SetDefaults(defaults)
		assert.NoError(t, err)
		assert.Len(t, ups.Failsafe, 1)
		assert.Equal(t, 3, ups.Failsafe[0].Retry.MaxAttempts)
		assert.Equal(t, Duration(5*time.Second), ups.Failsafe[0].Timeout.Duration)
	})

	t.Run("legacy matchMethod field is converted to a matcher", func(t *testing.T) {
		// Failsafe configs don't auto-prepend a catch-all-exclude (the include
		// semantic is implicit: matchers.Match returns false when no matcher
		// claims the request). So a single legacy matchMethod becomes exactly
		// one matcher.
		ups := &UpstreamConfig{
			Type:     UpstreamTypeEvm,
			Endpoint: "http://test/",
			Failsafe: []*FailsafeConfig{
				{
					MatchMethod: "eth_call",
					Timeout:     &TimeoutPolicyConfig{Duration: Duration(5 * time.Second)},
				},
			},
		}

		err := ups.SetDefaults(nil)
		assert.NoError(t, err)
		assert.Len(t, ups.Failsafe[0].Matchers, 1,
			"legacy MatchMethod should be converted to a single matcher")
		assert.Equal(t, "eth_call", ups.Failsafe[0].Matchers[0].Method)
		assert.Equal(t, MatcherInclude, ups.Failsafe[0].Matchers[0].Action)
	})

	t.Run("legacy matchFinality field is converted to a matcher with finality slice copy", func(t *testing.T) {
		// PR #388 review item from cursor-bot MED: convertLegacyMatchers must
		// deep-copy MatchFinality so subsequent mutations of either slice
		// don't corrupt the other. Resolved at common/defaults.go:2184.
		original := []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnfinalized}
		ups := &UpstreamConfig{
			Type:     UpstreamTypeEvm,
			Endpoint: "http://test/",
			Failsafe: []*FailsafeConfig{
				{
					MatchFinality: original,
					Timeout:       &TimeoutPolicyConfig{Duration: Duration(5 * time.Second)},
				},
			},
		}

		err := ups.SetDefaults(nil)
		assert.NoError(t, err)
		assert.Len(t, ups.Failsafe[0].Matchers, 1)
		assert.Equal(t, original, ups.Failsafe[0].Matchers[0].Finality)

		// Mutate the source slice — converted slice must NOT be affected.
		original[0] = DataFinalityStateFinalized
		assert.Equal(t, DataFinalityStateRealtime, ups.Failsafe[0].Matchers[0].Finality[0],
			"converted finality slice must be a deep copy")
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
