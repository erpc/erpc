package auth

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestAuthorizerShouldApplyToMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cfg    *common.AuthStrategyConfig
		method string
		want   bool
	}{
		{
			name:   "no filters allows method",
			cfg:    &common.AuthStrategyConfig{},
			method: "eth_call",
			want:   true,
		},
		{
			name: "ignore match blocks method",
			cfg: &common.AuthStrategyConfig{
				IgnoreMethods: []string{"eth_*"},
			},
			method: "eth_call",
			want:   false,
		},
		{
			name: "ignore miss allows method",
			cfg: &common.AuthStrategyConfig{
				IgnoreMethods: []string{"debug_*"},
			},
			method: "eth_call",
			want:   true,
		},
		{
			name: "allow match allows method",
			cfg: &common.AuthStrategyConfig{
				AllowMethods: []string{"eth_call", "eth_chainId"},
			},
			method: "eth_call",
			want:   true,
		},
		{
			name: "allow miss still allows method without ignore match",
			cfg: &common.AuthStrategyConfig{
				AllowMethods: []string{"eth_call", "eth_chainId"},
			},
			method: "debug_traceBlockByNumber",
			want:   true,
		},
		{
			name: "allow overrides ignore",
			cfg: &common.AuthStrategyConfig{
				IgnoreMethods: []string{"*"},
				AllowMethods:  []string{"eth_call"},
			},
			method: "eth_call",
			want:   true,
		},
		{
			name: "allow miss stays blocked with ignore all",
			cfg: &common.AuthStrategyConfig{
				IgnoreMethods: []string{"*"},
				AllowMethods:  []string{"eth_call"},
			},
			method: "eth_getLogs",
			want:   false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := &Authorizer{cfg: tt.cfg}
			require.Equal(t, tt.want, a.shouldApplyToMethod(tt.method))
		})
	}
}
