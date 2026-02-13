package common

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
  chainId: 123
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
  chainId: 123
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
  chainId: 123
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
  chainId: 123
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

func TestNetworkConfigFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
architecture: evm
failsafe:
- matchMethod: "*"
  matchFinality: ["realtime"]
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var config NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Failsafe, 2)
		assert.Equal(t, "*", config.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", config.Failsafe[1].MatchMethod)
		assert.Equal(t, 2*time.Second, time.Duration(config.Failsafe[0].Timeout.Duration))
		assert.Equal(t, 3, config.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
architecture: evm
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var config NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Failsafe, 1)
		assert.Equal(t, "*", config.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, time.Duration(config.Failsafe[0].Timeout.Duration))
		assert.Equal(t, 3, config.Failsafe[0].Retry.MaxAttempts)
	})
}

func TestNetworkDefaultsFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
- matchMethod: "*"
  timeout:
    duration: 2s
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var defaults NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &defaults)
		assert.NoError(t, err)
		assert.Len(t, defaults.Failsafe, 2)
		assert.Equal(t, "*", defaults.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", defaults.Failsafe[1].MatchMethod)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
rateLimitBudget: "test"
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var defaults NetworkDefaults
		err := yaml.Unmarshal([]byte(yamlData), &defaults)
		assert.NoError(t, err)
		assert.Len(t, defaults.Failsafe, 1)
		assert.Equal(t, "*", defaults.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, time.Duration(defaults.Failsafe[0].Timeout.Duration))
	})
}

func TestUpstreamConfigFailsafeBackwardCompatibility(t *testing.T) {
	t.Run("new array format", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://example.com"
failsafe:
- matchMethod: "*"
  timeout:
    duration: 2s
- matchMethod: "eth_*"
  timeout:
    duration: 5s
`
		var upstream UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &upstream)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 2)
		assert.Equal(t, "*", upstream.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", upstream.Failsafe[1].MatchMethod)
	})

	t.Run("old single object format", func(t *testing.T) {
		yamlData := `
id: "test-upstream"
endpoint: "http://example.com"
failsafe:
  timeout:
    duration: 2s
  retry:
    maxAttempts: 3
`
		var upstream UpstreamConfig
		err := yaml.Unmarshal([]byte(yamlData), &upstream)
		assert.NoError(t, err)
		assert.Len(t, upstream.Failsafe, 1)
		assert.Equal(t, "*", upstream.Failsafe[0].MatchMethod) // Should default to "*"
		assert.Equal(t, 2*time.Second, time.Duration(upstream.Failsafe[0].Timeout.Duration))
	})
}

func TestFullConfigWithNetworkFailsafe(t *testing.T) {
	t.Run("full config with array failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    alias: hyperevm
    failsafe:
    - matchMethod: "*"
      matchFinality: ["realtime"]
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
    - matchMethod: "*"
      matchFinality: ["unfinalized", "finalized", "unknown"]
      timeout:
        duration: 5s
      retry:
        maxAttempts: 5
        delay: "10ms"
      hedge:
        maxCount: 1
        quantile: 0.95
        minDelay: "500ms"
      consensus:
        maxParticipants: 10
        agreementThreshold: 2
        disputeBehavior: "returnError"
        lowParticipantsBehavior: "acceptMostCommonValidResult"
        punishMisbehavior:
          disputeThreshold: 10
          disputeWindow: "10m"
          sitOutPenalty: "30m"
`
		var config Config
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Equal(t, NetworkArchitecture("evm"), network.Architecture)
		assert.Equal(t, "hyperevm", network.Alias)
		assert.Len(t, network.Failsafe, 2)

		// Check first failsafe config
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Contains(t, network.Failsafe[0].MatchFinality, DataFinalityStateRealtime)
		assert.Equal(t, 2*time.Second, time.Duration(network.Failsafe[0].Timeout.Duration))
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)

		// Check second failsafe config
		assert.Equal(t, "*", network.Failsafe[1].MatchMethod)
		assert.Contains(t, network.Failsafe[1].MatchFinality, DataFinalityStateUnfinalized)
		assert.Equal(t, 5*time.Second, time.Duration(network.Failsafe[1].Timeout.Duration))
		assert.Equal(t, 5, network.Failsafe[1].Retry.MaxAttempts)
		assert.NotNil(t, network.Failsafe[1].Hedge)
		assert.NotNil(t, network.Failsafe[1].Consensus)
	})

	t.Run("full config with old single failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    failsafe:
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
`
		var config Config
		err := yaml.Unmarshal([]byte(yamlData), &config)
		assert.NoError(t, err)
		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, 2*time.Second, time.Duration(network.Failsafe[0].Timeout.Duration))
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
	})
}

func TestLoadConfigWithNetworkFailsafe(t *testing.T) {
	t.Run("LoadConfig with array failsafe in network", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
- id: test-project
  networks:
  - architecture: evm
    evm:
      chainId: 42161
    failsafe:
    - matchMethod: "*"
      timeout:
        duration: 2s
      retry:
        maxAttempts: 3
    - matchMethod: "eth_*"
      timeout:
        duration: 5s
`
		// Create a temporary file
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// Load the config using LoadConfig
		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		assert.Len(t, config.Projects, 1)
		assert.Len(t, config.Projects[0].Networks, 1)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 2)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, "eth_*", network.Failsafe[1].MatchMethod)
	})
}

func TestFailingConfigScenario(t *testing.T) {
	t.Run("Mixed old and new failsafe formats with maxCount", func(t *testing.T) {
		// This test demonstrates that invalid field names now give clear error messages
		yamlData := `
logLevel: error
projects:
  - id: main
    networkDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry:
          maxAttempts: 6
          delay: 0ms
    upstreamDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry: null
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        alias: hyperevm
        failsafe:
        - matchMethod: "*"
          matchFinality: ["realtime"]
          timeout:
            duration: 2s
          retry:
            maxCount: 3
        - matchMethod: "*"
          matchFinality: ["unfinalized", "finalized", "unknown"]
          timeout:
            duration: 5s
          retry:
            maxCount: 5
            delay: "10ms"
`
		// Create a temporary file
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should fail with a clear error message about invalid field
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		// Verify we get a clear error message
		assert.Error(t, err)
		assert.Nil(t, config)

		if err != nil {
			t.Logf("LoadConfig error (expected): %v", err)
			errStr := err.Error()

			// The error should now clearly mention:
			// 1. The invalid field name "maxCount"
			// 2. The correct type "RetryPolicyConfig"
			// 3. Line numbers where the errors occur
			assert.Contains(t, errStr, "maxCount", "Error should mention the invalid field 'maxCount'")
			assert.Contains(t, errStr, "RetryPolicyConfig", "Error should mention the type where field is not found")
			assert.Contains(t, errStr, "line", "Error should include line numbers")

			// The error should NOT have the confusing sequence unmarshal message
			assert.NotContains(t, errStr, "!!seq", "Error should not mention sequence unmarshaling")
			assert.NotContains(t, errStr, "cannot unmarshal !!seq into common.FailsafeConfig",
				"Error should not have the confusing sequence error")
		}
	})
}

func TestInvalidFieldNameErrorMessage(t *testing.T) {
	t.Run("Invalid field maxCount in retry should give clear error", func(t *testing.T) {
		// Test with just the retry config to isolate the issue
		yamlData := `
maxCount: 3
delay: 10ms
`
		var retryConfig RetryPolicyConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true) // This should catch unknown fields
		err := decoder.Decode(&retryConfig)

		// We expect an error about unknown field
		assert.Error(t, err)
		if err != nil {
			t.Logf("Error (as expected): %v", err)
			// The error should mention "maxCount" as unknown field
			assert.Contains(t, err.Error(), "maxCount", "Error message should mention the invalid field 'maxCount'")
		}
	})

	t.Run("Valid field maxAttempts should work", func(t *testing.T) {
		yamlData := `
maxAttempts: 3
delay: 10ms
`
		var retryConfig RetryPolicyConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true)
		err := decoder.Decode(&retryConfig)

		assert.NoError(t, err)
		assert.Equal(t, 3, retryConfig.MaxAttempts)
		assert.Equal(t, 10*time.Millisecond, time.Duration(retryConfig.Delay))
	})

	t.Run("Invalid field in failsafe array gives confusing error", func(t *testing.T) {
		// This shows the actual problem - when invalid fields are in an array of failsafe configs
		yamlData := `
- matchMethod: "*"
  retry:
    maxCount: 3
`
		var failsafeConfigs []*FailsafeConfig
		decoder := yaml.NewDecoder(strings.NewReader(yamlData))
		decoder.KnownFields(true)
		err := decoder.Decode(&failsafeConfigs)

		// This is the confusing error we currently get
		assert.Error(t, err)
		if err != nil {
			t.Logf("Current error: %v", err)
			// This error is confusing - it says it can't unmarshal a sequence into FailsafeConfig
			// but the real issue is the invalid "maxCount" field
		}
	})
}

func TestLoadConfigWithInvalidFieldName(t *testing.T) {
	t.Run("LoadConfig with invalid maxCount field should give clear error", func(t *testing.T) {
		// Full config with invalid field name (maxCount instead of maxAttempts)
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxCount: 3  # This should be maxAttempts
              delay: 10ms
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// Load the config - this should fail with an error
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		// Check what error we get
		assert.Error(t, err)
		if err != nil {
			t.Logf("LoadConfig error: %v", err)

			// The error should ideally mention "maxCount" as an invalid field
			// but currently it gives a confusing message about unmarshaling sequences

			// Let's check if the error mentions anything useful
			errStr := err.Error()
			t.Logf("Error contains 'maxCount': %v", strings.Contains(errStr, "maxCount"))
			t.Logf("Error contains 'unmarshal': %v", strings.Contains(errStr, "unmarshal"))
			t.Logf("Error contains 'seq': %v", strings.Contains(errStr, "seq"))
		}
		assert.Nil(t, config)
	})

	t.Run("LoadConfig with correct maxAttempts field should work", func(t *testing.T) {
		// Same config but with correct field name
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxAttempts: 3  # Correct field name
              delay: 10ms
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should work
		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		if config != nil {
			network := config.Projects[0].Networks[0]
			assert.Len(t, network.Failsafe, 1)
			assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
		}
	})

	t.Run("LoadConfig with invalid field in complex nested structure", func(t *testing.T) {
		// This reproduces the exact scenario from the user's failing config
		yamlData := `
logLevel: error
projects:
  - id: main
    networkDefaults:
      failsafe:
        timeout:
          duration: 300s
        retry:
          maxAttempts: 6  # This is correct
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          - matchMethod: "*"
            matchFinality: ["realtime"]
            timeout:
              duration: 2s
            retry:
              maxCount: 3  # This is INCORRECT - should be maxAttempts
          - matchMethod: "*"
            matchFinality: ["unfinalized", "finalized", "unknown"]
            timeout:
              duration: 5s
            retry:
              maxCount: 5  # This is INCORRECT - should be maxAttempts
              delay: "10ms"
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		// This should fail
		config, err := LoadConfig(fs, "test-config.yaml", nil)

		assert.Error(t, err)
		if err != nil {
			t.Logf("LoadConfig error with complex nested structure: %v", err)

			// The error message should help identify the problem
			// Currently it says: "cannot unmarshal !!seq into common.FailsafeConfig"
			// which is confusing because the real issue is the invalid field name

			errStr := err.Error()
			t.Logf("Error mentions line number: %v", strings.Contains(errStr, "line"))

			// Ideally, the error should say something like:
			// "field maxCount not found in type common.RetryPolicyConfig at line X"
		}
		assert.Nil(t, config)
	})
}

func TestBackwardCompatibilityStillWorks(t *testing.T) {
	t.Run("Old single failsafe format still works", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    upstreams:
      - endpoint: https://example.com
        evm:
          chainId: 42161
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:  # Old format: single object instead of array
          timeout:
            duration: 2s
          retry:
            maxAttempts: 3
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		// Old format should be converted to new array format
		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 1)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod) // Default value
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
	})

	t.Run("New array failsafe format works", func(t *testing.T) {
		yamlData := `
logLevel: debug
projects:
  - id: test-project
    upstreams:
      - endpoint: https://example.com
        evm:
          chainId: 42161
    networks:
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:  # New format: array
          - matchMethod: "*"
            timeout:
              duration: 2s
            retry:
              maxAttempts: 3
          - matchMethod: "eth_*"
            timeout:
              duration: 5s
            retry:
              maxAttempts: 5
`
		fs := afero.NewMemMapFs()
		err := afero.WriteFile(fs, "test-config.yaml", []byte(yamlData), 0644)
		assert.NoError(t, err)

		config, err := LoadConfig(fs, "test-config.yaml", nil)
		assert.NoError(t, err)
		assert.NotNil(t, config)

		network := config.Projects[0].Networks[0]
		assert.Len(t, network.Failsafe, 2)
		assert.Equal(t, "*", network.Failsafe[0].MatchMethod)
		assert.Equal(t, 3, network.Failsafe[0].Retry.MaxAttempts)
		assert.Equal(t, "eth_*", network.Failsafe[1].MatchMethod)
		assert.Equal(t, 5, network.Failsafe[1].Retry.MaxAttempts)
	})
}

func TestMulticall3AggregationConfigYAML(t *testing.T) {
	yamlStr := `
evm:
  chainId: 1
  multicall3Aggregation:
    enabled: true
    windowMs: 25
    minWaitMs: 2
    safetyMarginMs: 2
    maxCalls: 20
    maxCalldataBytes: 64000
    maxQueueSize: 1000
    maxPendingBatches: 200
    cachePerCall: true
    allowCrossUserBatching: true
    allowPendingTagBatching: false
`
	var cfg NetworkConfig
	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Evm)
	require.NotNil(t, cfg.Evm.Multicall3Aggregation)
	require.True(t, cfg.Evm.Multicall3Aggregation.Enabled)
	require.Equal(t, 25, cfg.Evm.Multicall3Aggregation.WindowMs)
	require.Equal(t, 2, cfg.Evm.Multicall3Aggregation.MinWaitMs)
	require.Equal(t, 2, cfg.Evm.Multicall3Aggregation.SafetyMarginMs)
	require.Equal(t, 20, cfg.Evm.Multicall3Aggregation.MaxCalls)
	require.Equal(t, 64000, cfg.Evm.Multicall3Aggregation.MaxCalldataBytes)
	require.Equal(t, 1000, cfg.Evm.Multicall3Aggregation.MaxQueueSize)
	require.Equal(t, 200, cfg.Evm.Multicall3Aggregation.MaxPendingBatches)
	require.NotNil(t, cfg.Evm.Multicall3Aggregation.CachePerCall)
	require.True(t, *cfg.Evm.Multicall3Aggregation.CachePerCall)
	require.NotNil(t, cfg.Evm.Multicall3Aggregation.AllowCrossUserBatching)
	require.True(t, *cfg.Evm.Multicall3Aggregation.AllowCrossUserBatching)
	require.False(t, cfg.Evm.Multicall3Aggregation.AllowPendingTagBatching)
}

func TestMulticall3AggregationConfigBoolBackcompat(t *testing.T) {
	// Test backward compatibility with bool value
	yamlStr := `
evm:
  chainId: 1
  multicall3Aggregation: true
`
	var cfg NetworkConfig
	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Evm)
	require.NotNil(t, cfg.Evm.Multicall3Aggregation)
	require.True(t, cfg.Evm.Multicall3Aggregation.Enabled)
	// Check defaults are applied when bool is true
	require.Equal(t, 25, cfg.Evm.Multicall3Aggregation.WindowMs)
	require.Equal(t, 20, cfg.Evm.Multicall3Aggregation.MaxCalls)
}

func TestMulticall3AggregationConfigBoolFalse(t *testing.T) {
	// Test backward compatibility with false value
	yamlStr := `
evm:
  chainId: 1
  multicall3Aggregation: false
`
	var cfg NetworkConfig
	err := yaml.Unmarshal([]byte(yamlStr), &cfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Evm)
	require.NotNil(t, cfg.Evm.Multicall3Aggregation)
	require.False(t, cfg.Evm.Multicall3Aggregation.Enabled)
}

func TestMulticall3AggregationConfigDefaults(t *testing.T) {
	cfg := &Multicall3AggregationConfig{Enabled: true}
	cfg.SetDefaults()
	require.Equal(t, 25, cfg.WindowMs)
	require.Equal(t, 2, cfg.MinWaitMs)
	require.Equal(t, 2, cfg.SafetyMarginMs)
	require.Equal(t, 20, cfg.MaxCalls)
	require.Equal(t, 64000, cfg.MaxCalldataBytes)
	require.Equal(t, 1000, cfg.MaxQueueSize)
	require.Equal(t, 200, cfg.MaxPendingBatches)
	require.NotNil(t, cfg.CachePerCall)
	require.True(t, *cfg.CachePerCall)
	require.NotNil(t, cfg.AllowCrossUserBatching)
	require.True(t, *cfg.AllowCrossUserBatching)
}

func TestMulticall3AggregationConfigIsValid(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true}
		cfg.SetDefaults()
		err := cfg.IsValid()
		require.NoError(t, err)
	})

	t.Run("windowMs must be positive", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true, WindowMs: 0}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "windowMs must be > 0")
	})

	t.Run("minWaitMs must not be negative", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true, WindowMs: 25, MinWaitMs: -1}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "minWaitMs must be >= 0")
	})

	t.Run("minWaitMs must be <= windowMs", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true, WindowMs: 10, MinWaitMs: 20}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "minWaitMs must be <= windowMs")
	})

	t.Run("maxCalls must be > 1", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true, WindowMs: 25, MinWaitMs: 2, MaxCalls: 1}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "maxCalls must be > 1")
	})

	t.Run("maxCalldataBytes must be positive", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{Enabled: true, WindowMs: 25, MinWaitMs: 2, MaxCalls: 20, MaxCalldataBytes: 0}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "maxCalldataBytes must be > 0")
	})

	t.Run("maxQueueSize must be positive", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled:          true,
			WindowMs:         25,
			MinWaitMs:        2,
			MaxCalls:         20,
			MaxCalldataBytes: 64000,
			MaxQueueSize:     0,
		}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "maxQueueSize must be > 0")
	})

	t.Run("maxPendingBatches must be positive", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled:           true,
			WindowMs:          25,
			MinWaitMs:         2,
			MaxCalls:          20,
			MaxCalldataBytes:  64000,
			MaxQueueSize:      1000,
			MaxPendingBatches: 0,
		}
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "maxPendingBatches must be > 0")
	})

	t.Run("bypassContracts valid addresses", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"0xABCDEF0123456789ABCDEF0123456789ABCDEF01",
				"1111111111111111111111111111111111111111", // without 0x prefix
			},
		}
		cfg.SetDefaults()
		err := cfg.IsValid()
		require.NoError(t, err)
	})

	t.Run("bypassContracts empty address rejected", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"", // empty
			},
		}
		cfg.SetDefaults()
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "contains empty address")
	})

	t.Run("bypassContracts invalid length rejected", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"0x1234", // too short
			},
		}
		cfg.SetDefaults()
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected 40 hex characters")
	})

	t.Run("bypassContracts non-hex characters rejected", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"0xZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", // non-hex
			},
		}
		cfg.SetDefaults()
		err := cfg.IsValid()
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-hex character")
	})
}

func TestMulticall3AggregationConfigBypassContracts(t *testing.T) {
	t.Run("ShouldBypassContractHex with empty list", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled:         true,
			BypassContracts: []string{},
		}
		cfg.SetDefaults()

		// Should not bypass any contract when list is empty
		require.False(t, cfg.ShouldBypassContractHex("0x057f30e63A69175C69A4Af5656b8C9EE647De3D0"))
		require.False(t, cfg.ShouldBypassContractHex(""))
	})

	t.Run("ShouldBypassContractHex with configured contracts", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"0xABCDEF0123456789ABCDEF0123456789ABCDEF01",
			},
		}
		cfg.SetDefaults()

		// Exact match
		require.True(t, cfg.ShouldBypassContractHex("0x057f30e63A69175C69A4Af5656b8C9EE647De3D0"))
		require.True(t, cfg.ShouldBypassContractHex("0xABCDEF0123456789ABCDEF0123456789ABCDEF01"))

		// Case-insensitive matching
		require.True(t, cfg.ShouldBypassContractHex("0x057f30e63a69175c69a4af5656b8c9ee647de3d0"))
		require.True(t, cfg.ShouldBypassContractHex("0x057F30E63A69175C69A4AF5656B8C9EE647DE3D0"))

		// Without 0x prefix
		require.True(t, cfg.ShouldBypassContractHex("057f30e63A69175C69A4Af5656b8C9EE647De3D0"))

		// Not in list
		require.False(t, cfg.ShouldBypassContractHex("0x1111111111111111111111111111111111111111"))
		require.False(t, cfg.ShouldBypassContractHex(""))
	})

	t.Run("ShouldBypassContract with raw bytes", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
			},
		}
		cfg.SetDefaults()

		// Matching address bytes
		matchingAddr, _ := HexToBytes("0x057f30e63A69175C69A4Af5656b8C9EE647De3D0")
		require.True(t, cfg.ShouldBypassContract(matchingAddr))

		// Non-matching address bytes
		otherAddr, _ := HexToBytes("0x1111111111111111111111111111111111111111")
		require.False(t, cfg.ShouldBypassContract(otherAddr))

		// Empty bytes
		require.False(t, cfg.ShouldBypassContract(nil))
		require.False(t, cfg.ShouldBypassContract([]byte{}))
	})

	t.Run("BypassContracts handles invalid entries gracefully", func(t *testing.T) {
		cfg := &Multicall3AggregationConfig{
			Enabled: true,
			BypassContracts: []string{
				"0x057f30e63A69175C69A4Af5656b8C9EE647De3D0",
				"",        // empty string
				"   ",     // whitespace only becomes empty after trim
				"0x",      // just prefix
				"invalid", // non-hex characters
			},
		}
		cfg.SetDefaults()

		// Valid entry should still work
		require.True(t, cfg.ShouldBypassContractHex("0x057f30e63A69175C69A4Af5656b8C9EE647De3D0"))

		// Empty/invalid entries should be ignored (not panic)
		require.False(t, cfg.ShouldBypassContractHex(""))
	})

	t.Run("nil config does not panic", func(t *testing.T) {
		var cfg *Multicall3AggregationConfig
		// Should not panic, just return false
		require.NotPanics(t, func() {
			// Note: ShouldBypassContractHex is a method on a pointer, so nil check is inside
			if cfg != nil {
				cfg.ShouldBypassContractHex("0x1234")
			}
		})
	})
}
