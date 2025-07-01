package common

import (
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func TestLoadConfig_FailToReadFile(t *testing.T) {
	fs := afero.NewMemMapFs()
	_, err := LoadConfig(fs, "nonexistent.yaml", &DefaultOptions{})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestLoadConfig_InvalidYaml(t *testing.T) {
	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("invalid yaml")

	_, err = LoadConfig(fs, cfg.Name(), &DefaultOptions{})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestLoadConfig_ValidYaml(t *testing.T) {
	fs := afero.NewMemMapFs()
	cfg, err := afero.TempFile(fs, "", "erpc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString(`
logLevel: DEBUG
`)

	_, err = LoadConfig(fs, cfg.Name(), &DefaultOptions{})
	if err != nil {
		t.Error(err)
	}
}

func TestFailsafeConfigBackwardCompatibility(t *testing.T) {
	t.Run("NetworkDefaults old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  retry:
    maxAttempts: 3
`
		var nd NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &nd)
		assert.NoError(t, err)

		// Should convert to array with one element
		assert.Len(t, nd.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", nd.Failsafe[0].MatchMethod)
		assert.NotNil(t, nd.Failsafe[0].Retry)
		assert.Equal(t, 3, nd.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("NetworkDefaults old format with non-empty MatchMethod", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  matchMethod: "eth_*"
  retry:
    maxAttempts: 3
`
		var nd NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &nd)
		assert.NoError(t, err)

		assert.Len(t, nd.Failsafe, 1)
		// Existing MatchMethod should be preserved
		assert.Equal(t, "eth_*", nd.Failsafe[0].MatchMethod)
	})

	t.Run("UpstreamConfig old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://test.com"
failsafe:
  retry:
    maxAttempts: 3
`
		var uc UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &uc)
		assert.NoError(t, err)

		assert.Len(t, uc.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", uc.Failsafe[0].MatchMethod)
	})

	t.Run("NetworkConfig old format with empty MatchMethod", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 1
failsafe:
  circuitBreaker:
    failureThresholdCount: 5
    failureThresholdCapacity: 10
    halfOpenAfter: 10s
    successThresholdCount: 3
    successThresholdCapacity: 5
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 1)
		// Empty MatchMethod should be converted to "*"
		assert.Equal(t, "*", nc.Failsafe[0].MatchMethod)
		assert.NotNil(t, nc.Failsafe[0].CircuitBreaker)
	})

	t.Run("New array format with MatchFinality", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 1
failsafe:
  - matchMethod: "eth_call"
    matchFinality: ["realtime", "unknown"]  # realtime and unknown finality states
    retry:
      maxAttempts: 5
  - matchMethod: "eth_getBalance"
    matchFinality: ["unfinalized"]  # unfinalized state
    retry:
      maxAttempts: 2
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 2)
		assert.Equal(t, "eth_call", nc.Failsafe[0].MatchMethod)
		assert.Equal(t, []DataFinalityState{DataFinalityStateRealtime, DataFinalityStateUnknown}, nc.Failsafe[0].MatchFinality)
		assert.Equal(t, 5, nc.Failsafe[0].Retry.MaxAttempts)

		assert.Equal(t, "eth_getBalance", nc.Failsafe[1].MatchMethod)
		assert.Equal(t, []DataFinalityState{DataFinalityStateUnfinalized}, nc.Failsafe[1].MatchFinality)
		assert.Equal(t, 2, nc.Failsafe[1].Retry.MaxAttempts)
	})

	t.Run("FailsafeConfig validation rejects empty MatchMethod", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "", // empty should be rejected
			Retry: &RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}

		err := fs.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failsafe.matchMethod cannot be empty")
	})

	t.Run("FailsafeConfig validation accepts wildcard", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "*",
			Retry: &RetryPolicyConfig{
				MaxAttempts:     3,
				BackoffFactor:   2.0,
				BackoffMaxDelay: Duration(10 * time.Second),
			},
		}

		err := fs.Validate()
		assert.NoError(t, err)
	})

	t.Run("FailsafeConfig SetDefaults sets MatchMethod to wildcard", func(t *testing.T) {
		fs := &FailsafeConfig{
			MatchMethod: "", // empty
			Retry: &RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}

		err := fs.SetDefaults(nil)
		assert.NoError(t, err)
		assert.Equal(t, "*", fs.MatchMethod)
	})

	t.Run("MatchFinality with all string values", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 1
failsafe:
  - matchMethod: "eth_*"
    matchFinality: ["finalized", "unfinalized", "realtime", "unknown"]
    retry:
      maxAttempts: 3
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 1)
		assert.Equal(t, "eth_*", nc.Failsafe[0].MatchMethod)
		assert.Equal(t, []DataFinalityState{
			DataFinalityStateFinalized,
			DataFinalityStateUnfinalized,
			DataFinalityStateRealtime,
			DataFinalityStateUnknown,
		}, nc.Failsafe[0].MatchFinality)
	})

	t.Run("MatchFinality with mixed case strings", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 1
failsafe:
  - matchMethod: "eth_call"
    matchFinality: ["Finalized", "UNFINALIZED", "ReaLTime"]
    retry:
      maxAttempts: 3
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		// Should handle case-insensitive parsing
		assert.Len(t, nc.Failsafe, 1)
		assert.Equal(t, []DataFinalityState{
			DataFinalityStateFinalized,
			DataFinalityStateUnfinalized,
			DataFinalityStateRealtime,
		}, nc.Failsafe[0].MatchFinality)
	})
}
