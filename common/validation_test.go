package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrpcLoadBalancingConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		wantErr string
	}{
		{
			name:   "round_robin is valid",
			policy: "round_robin",
		},
		{
			name:   "pick_first is valid",
			policy: "pick_first",
		},
		{
			name:    "empty string is invalid",
			policy:  "",
			wantErr: "must be 'round_robin' or 'pick_first', got ''",
		},
		{
			name:    "typo is invalid",
			policy:  "rund_robin",
			wantErr: "must be 'round_robin' or 'pick_first', got 'rund_robin'",
		},
		{
			name:    "unknown policy is invalid",
			policy:  "least_connections",
			wantErr: "must be 'round_robin' or 'pick_first', got 'least_connections'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &GrpcLoadBalancingConfig{Policy: tt.policy}
			err := cfg.Validate()
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGrpcUpstreamConfig_Validate(t *testing.T) {
	t.Run("nil load balancing is valid", func(t *testing.T) {
		cfg := &GrpcUpstreamConfig{}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("valid load balancing passes", func(t *testing.T) {
		cfg := &GrpcUpstreamConfig{
			LoadBalancing: &GrpcLoadBalancingConfig{Policy: "round_robin"},
		}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("invalid load balancing propagates error", func(t *testing.T) {
		cfg := &GrpcUpstreamConfig{
			LoadBalancing: &GrpcLoadBalancingConfig{Policy: "bad"},
		}
		err := cfg.Validate()
		assert.ErrorContains(t, err, "must be 'round_robin' or 'pick_first'")
	})
}
