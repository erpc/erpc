package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeTestMaxTimeout keeps this file self-contained: borrowing a helper from
// another test file would break the moment that file is refactored.
func probeTestMaxTimeout() *Duration {
	d := Duration(30 * time.Second)
	return &d
}

func probeTestConfig(probe ProbeMode) *Config {
	up := &UpstreamConfig{Id: "u1", Endpoint: "http://rpc.example"}
	if probe != "" {
		up.Routing = &UpstreamRoutingConfig{Probe: probe}
	}
	return &Config{
		Server:      &ServerConfig{MaxTimeout: probeTestMaxTimeout()},
		HealthCheck: &HealthCheckConfig{},
		Projects: []*ProjectConfig{{
			Id:        "main",
			Upstreams: []*UpstreamConfig{up},
		}},
	}
}

// A YAML boolean in routing.probe used to unmarshal to ProbeMode("false"), which
// is not ProbeModeOff, so probing stayed enabled while the config read as off.
func TestUpstreamRoutingProbeValidation(t *testing.T) {
	require.NoError(t, probeTestConfig("").Validate())
	require.NoError(t, probeTestConfig(ProbeModeOn).Validate())
	require.NoError(t, probeTestConfig(ProbeModeOff).Validate())

	for _, bad := range []ProbeMode{"false", "true", "offf", "OFF", "0"} {
		t.Run(string(bad), func(t *testing.T) {
			err := probeTestConfig(bad).Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "routing.probe")
			assert.Contains(t, err.Error(), "u1")
		})
	}
}

// The value the issue reported: `probe: false` in YAML.
func TestUpstreamRoutingProbeYamlBooleanRejected(t *testing.T) {
	cfg := &Config{
		Server:      &ServerConfig{MaxTimeout: probeTestMaxTimeout()},
		HealthCheck: &HealthCheckConfig{},
		Projects: []*ProjectConfig{{
			Id: "main",
			Upstreams: []*UpstreamConfig{{
				Id:       "paid-vendor",
				Endpoint: "http://rpc.example",
			}},
		}},
	}
	// Simulate what yaml.v3 does with `probe: false`.
	cfg.Projects[0].Upstreams[0].Routing = &UpstreamRoutingConfig{Probe: ProbeMode("false")}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paid-vendor")
	assert.Contains(t, err.Error(), `routing.probe 'false'`)
	assert.Contains(t, err.Error(), "must be one of")
}
