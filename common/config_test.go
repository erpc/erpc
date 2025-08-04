package common

import (
	"strings"
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

	t.Run("FailsafeConfig with matchers", func(t *testing.T) {
		yamlData := `
architecture: "evm"
evm:
  chainId: 1
failsafe:
  - matchers:
    - network: "evm:1"
      method: "eth_*"
      finality: ["finalized"]
      action: "include"
    retry:
      maxAttempts: 3
`
		var nc NetworkConfig
		err := yaml.Unmarshal([]byte(yamlData), &nc)
		assert.NoError(t, err)

		assert.Len(t, nc.Failsafe, 1)
		assert.Len(t, nc.Failsafe[0].Matchers, 1)
		assert.Equal(t, "evm:1", nc.Failsafe[0].Matchers[0].Network)
		assert.Equal(t, "eth_*", nc.Failsafe[0].Matchers[0].Method)
		assert.Equal(t, []DataFinalityState{DataFinalityStateFinalized}, nc.Failsafe[0].Matchers[0].Finality)
		assert.Equal(t, MatcherInclude, nc.Failsafe[0].Matchers[0].Action)
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

func TestMatchersValidate(t *testing.T) {
	t.Run("only include rules - should add catch-all exclude", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "eth_call",
				Action: MatcherInclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 2)
		assert.Equal(t, MatcherExclude, result[0].Action) // catch-all exclude added at beginning
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherInclude, result[1].Action) // original rule
	})

	t.Run("only exclude rules - should add catch-all include", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 2)
		assert.Equal(t, MatcherInclude, result[0].Action) // catch-all include added at beginning
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherExclude, result[1].Action) // original rule
		assert.Equal(t, "debug_*", result[1].Method)
	})

	t.Run("mixed rules with valid first action - should pass through", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*",
				Action: MatcherInclude, // First rule determines starting point
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 2) // No catch-all rules added for mixed
		assert.Equal(t, MatcherInclude, result[0].Action)
		assert.Equal(t, MatcherExclude, result[1].Action)
	})

	t.Run("mixed rules with missing first action - should default to include and pass", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*",
				Action: "", // Empty action should default to include
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
			{
				Method: "eth_*",
				Action: MatcherInclude, // This makes it mixed
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err, "Should pass since empty action defaults to include")
		assert.NotNil(t, result)
		assert.Equal(t, MatcherInclude, result[0].Action, "First matcher should have defaulted to include")
	})

	t.Run("mixed rules with non-catch-all first rule - should error", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "eth_call", // Not a catch-all method
				Action: MatcherInclude,
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FIRST rule must be a catch-all rule")
		assert.Contains(t, err.Error(), "method=\"eth_call\"")
		assert.Nil(t, result)
	})

	t.Run("mixed rules with catch-all first rule (method='*') - should pass", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*", // Catch-all method
				Action: MatcherInclude,
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Equal(t, matchers, result) // Should pass through unchanged
	})

	t.Run("mixed rules with catch-all first rule (method=empty) - should pass", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "", // Empty method means catch-all
				Action: MatcherInclude,
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Equal(t, matchers, result) // Should pass through unchanged
	})

	t.Run("mixed rules with params in first rule - should error", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*",
				Action: MatcherInclude,
				Params: []interface{}{"some", "params"}, // Not allowed for catch-all
			},
			{
				Method: "debug_*",
				Action: MatcherExclude,
			},
		}

		result, err := validateMatchers(matchers)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "FIRST rule must be a catch-all rule")
		assert.Contains(t, err.Error(), "params=2")
		assert.Nil(t, result)
	})

	t.Run("empty matchers - should return as-is", func(t *testing.T) {
		var matchers []*MatcherConfig

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 0)
	})

	t.Run("auto-generated default matcher - should NOT get global override", func(t *testing.T) {
		// This simulates what ConvertFailsafeLegacyMatchers creates when no explicit matchers are provided
		matchers := []*MatcherConfig{
			{
				Method: "*",
				Action: MatcherInclude,
				// Network, Params, Finality are all empty/nil by default
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 1, "Auto-generated default matcher should not get catch-all exclude added")
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherInclude, result[0].Action)
	})

	t.Run("matchers with missing action - should default to include", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "eth_call",
				// Action is intentionally missing (empty string)
			},
			{
				Method: "eth_getBalance",
				// Action is also missing - should default to include
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 3) // Original 2 + 1 catch-all exclude (since all are includes)

		// First matcher should have defaulted to include
		assert.Equal(t, "eth_call", result[1].Method) // Index 1 because catch-all exclude is added at 0
		assert.Equal(t, MatcherInclude, result[1].Action)

		// Second matcher should have defaulted to include
		assert.Equal(t, "eth_getBalance", result[2].Method)
		assert.Equal(t, MatcherInclude, result[2].Action)

		// Catch-all exclude should be added first
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherExclude, result[0].Action)
	})

	t.Run("matcher with invalid action - should error", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "eth_call",
				Action: "invalid_action", // Invalid action
			},
		}

		result, err := validateMatchers(matchers)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "matcher[0] has invalid action 'invalid_action'")
		assert.Contains(t, err.Error(), "must be either 'include' or 'exclude'")
		assert.Nil(t, result)
	})

	t.Run("multiple matchers with mixed missing and invalid actions", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "eth_call",
				// Action missing - should default to include
			},
			{
				Method: "eth_getBalance",
				Action: "wrong", // Invalid action - should error
			},
		}

		result, err := validateMatchers(matchers)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "matcher[1] has invalid action 'wrong'")
		assert.Nil(t, result)
	})

	t.Run("mixed include/exclude with missing action in catch-all first rule", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*", // Catch-all first rule
				// Action missing - should default to include
			},
			{
				Method: "debug_*",
				Action: MatcherExclude, // This makes it mixed
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err, "Should pass with catch-all first rule")
		assert.Len(t, result, 2)

		// First matcher should have defaulted to include
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherInclude, result[0].Action)

		// Second matcher should remain as exclude
		assert.Equal(t, "debug_*", result[1].Method)
		assert.Equal(t, MatcherExclude, result[1].Action)
	})

	t.Run("single matcher with missing action - should default to include and skip global override", func(t *testing.T) {
		matchers := []*MatcherConfig{
			{
				Method: "*",
				// Action missing - should default to include
				// This should be recognized as auto-generated default
			},
		}

		result, err := validateMatchers(matchers)
		assert.NoError(t, err)
		assert.Len(t, result, 1, "Should not add catch-all exclude for auto-generated default")
		assert.Equal(t, "*", result[0].Method)
		assert.Equal(t, MatcherInclude, result[0].Action)
	})
}

func TestFailsafeValidation(t *testing.T) {
	t.Run("single policy with include matchers", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				Matchers: []*MatcherConfig{
					{
						Method: "eth_call",
						Action: MatcherInclude,
					},
				},
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.NoError(t, err, "Policy %d should validate successfully", i)
			// Check that catch-all exclude was added
			assert.Len(t, policy.Matchers, 2)
			assert.Equal(t, MatcherExclude, policy.Matchers[0].Action)
			assert.Equal(t, "*", policy.Matchers[0].Method)
		}
	})

	t.Run("multiple policies each with their own matchers", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				Matchers: []*MatcherConfig{
					{
						Method: "eth_call",
						Action: MatcherInclude,
					},
				},
			},
			{
				Matchers: []*MatcherConfig{
					{
						Method: "debug_*",
						Action: MatcherExclude,
					},
				},
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.NoError(t, err, "Policy %d should validate successfully", i)
		}

		// First policy should have catch-all exclude added
		assert.Len(t, policies[0].Matchers, 2)
		assert.Equal(t, MatcherExclude, policies[0].Matchers[0].Action)

		// Second policy should have catch-all include added at beginning
		assert.Len(t, policies[1].Matchers, 2)
		assert.Equal(t, MatcherInclude, policies[1].Matchers[0].Action)
	})

	t.Run("legacy mode with matchMethod", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				MatchMethod: "eth_*",
				// No Matchers defined - should use legacy mode
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.NoError(t, err, "Policy %d should validate successfully", i)
		}
	})

	t.Run("multiple policies with truly invalid action values", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				Matchers: []*MatcherConfig{
					{
						Method: "*",
						Action: "invalid_action", // Truly invalid action value
					},
					{
						Method: "debug_*",
						Action: MatcherExclude,
					},
				},
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.Error(t, err, "Policy %d should fail validation", i)
			assert.Contains(t, err.Error(), "invalid action 'invalid_action'")
		}
	})

	t.Run("mixed rules with non-catch-all first rule should error", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				Matchers: []*MatcherConfig{
					{
						Method: "eth_call", // Not a catch-all
						Action: MatcherInclude,
					},
					{
						Method: "debug_*",
						Action: MatcherExclude,
					},
				},
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.Error(t, err, "Policy %d should fail validation", i)
			assert.Contains(t, err.Error(), "FIRST rule must be a catch-all rule")
		}
	})

	t.Run("mixed rules with valid catch-all first rule should pass", func(t *testing.T) {
		policies := []*FailsafeConfig{
			{
				Matchers: []*MatcherConfig{
					{
						Method: "*", // Valid catch-all
						Action: MatcherInclude,
					},
					{
						Method: "debug_*",
						Action: MatcherExclude,
					},
				},
			},
		}

		for i, policy := range policies {
			err := policy.Validate()
			assert.NoError(t, err, "Policy %d should validate successfully", i)
		}
	})

	t.Run("legacy mode with empty matchMethod should create catch-all matcher", func(t *testing.T) {
		policy := &FailsafeConfig{
			MatchMethod: "", // Empty matchMethod with no Matchers - should mean "match all"
		}

		// First convert legacy matchers
		policy.ConvertFailsafeLegacyMatchers()

		// Then validate
		err := policy.Validate()
		assert.NoError(t, err, "Empty matchMethod should be valid (means match all)")

		// Should create a default catch-all matcher since no legacy fields were present
		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, "*", policy.Matchers[0].Method)
		assert.Equal(t, MatcherInclude, policy.Matchers[0].Action)
	})

	t.Run("legacy mode with empty matchMethod but with finality should apply global override", func(t *testing.T) {
		policy := &FailsafeConfig{
			MatchMethod:   "", // Empty method - should match all methods
			MatchFinality: []DataFinalityState{DataFinalityStateFinalized},
		}

		// First convert legacy matchers
		policy.ConvertFailsafeLegacyMatchers()

		// Then validate
		err := policy.Validate()
		assert.NoError(t, err, "Empty matchMethod with finality should be valid")

		// Should create a matcher with finality constraint + global override catch-all exclude
		// This is because a matcher with specific finality is NOT a default catch-all
		assert.Len(t, policy.Matchers, 2)

		// First matcher should be the catch-all exclude (processed last)
		assert.Equal(t, "*", policy.Matchers[0].Method)
		assert.Equal(t, MatcherExclude, policy.Matchers[0].Action)
		assert.Len(t, policy.Matchers[0].Finality, 0)

		// Second matcher should be the original finality-specific matcher
		assert.Equal(t, "", policy.Matchers[1].Method) // Empty means match all methods
		assert.Equal(t, MatcherInclude, policy.Matchers[1].Action)
		assert.Len(t, policy.Matchers[1].Finality, 1)
		assert.Equal(t, DataFinalityStateFinalized, policy.Matchers[1].Finality[0])
	})
}

func TestConvertCacheLegacyMatchers(t *testing.T) {
	t.Run("legacy fields are converted to matchers", func(t *testing.T) {
		policy := &CachePolicyConfig{
			Network:  "1",
			Method:   "eth_call",
			Params:   []interface{}{"0x123"},
			Finality: DataFinalityStateFinalized,
			Empty:    CacheEmptyBehaviorIgnore,
		}

		policy.ConvertCacheLegacyMatchers()

		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, "1", policy.Matchers[0].Network)
		assert.Equal(t, "eth_call", policy.Matchers[0].Method)
		assert.Equal(t, []interface{}{"0x123"}, policy.Matchers[0].Params)
		assert.Equal(t, []DataFinalityState{DataFinalityStateFinalized}, policy.Matchers[0].Finality)
		assert.Equal(t, CacheEmptyBehaviorIgnore, policy.Matchers[0].Empty)
		assert.Equal(t, MatcherInclude, policy.Matchers[0].Action)
	})

	t.Run("empty legacy fields create default catch-all include matcher", func(t *testing.T) {
		policy := &CachePolicyConfig{}

		policy.ConvertCacheLegacyMatchers()

		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, "*", policy.Matchers[0].Method)
		assert.Equal(t, MatcherInclude, policy.Matchers[0].Action)
	})

	t.Run("existing matchers are not modified", func(t *testing.T) {
		policy := &CachePolicyConfig{
			Network: "1",
			Method:  "eth_call",
			Matchers: []*MatcherConfig{
				{
					Method: "eth_getBalance",
					Action: MatcherInclude,
				},
			},
		}

		policy.ConvertCacheLegacyMatchers()

		// Should still have only one matcher - the original
		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, "eth_getBalance", policy.Matchers[0].Method)
	})

	t.Run("partial legacy fields are converted correctly", func(t *testing.T) {
		policy := &CachePolicyConfig{
			Network: "1",
			Method:  "eth_*",
			// Finality and Empty are zero values
		}

		policy.ConvertCacheLegacyMatchers()

		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, "1", policy.Matchers[0].Network)
		assert.Equal(t, "eth_*", policy.Matchers[0].Method)
		assert.Equal(t, []DataFinalityState{DataFinalityStateFinalized}, policy.Matchers[0].Finality) // Zero value (finalized) is included
		assert.Equal(t, CacheEmptyBehavior(0), policy.Matchers[0].Empty)                              // Zero value passed through
		assert.Equal(t, MatcherInclude, policy.Matchers[0].Action)
	})

	t.Run("params are deep copied", func(t *testing.T) {
		originalParams := []interface{}{"0x123", map[string]interface{}{"key": "value"}}
		policy := &CachePolicyConfig{
			Params: originalParams,
		}

		policy.ConvertCacheLegacyMatchers()

		assert.Len(t, policy.Matchers, 1)
		assert.Equal(t, originalParams, policy.Matchers[0].Params)

		// Modify original params to ensure deep copy
		originalParams[0] = "0x456"
		assert.Equal(t, "0x123", policy.Matchers[0].Params[0])
	})
}
