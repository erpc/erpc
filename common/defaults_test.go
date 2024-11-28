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

		assert.NotNil(t, network.Failsafe, "Failsafe should be defined")
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
				Delay:           sysDefCfg.Failsafe.Retry.Delay,
				BackoffMaxDelay: sysDefCfg.Failsafe.Retry.BackoffMaxDelay,
				BackoffFactor:   sysDefCfg.Failsafe.Retry.BackoffFactor,
				Jitter:          sysDefCfg.Failsafe.Retry.Jitter,
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
