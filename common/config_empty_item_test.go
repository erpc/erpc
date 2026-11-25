package common

import (
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// loadConfigString runs LoadConfig on an in-memory YAML file. A panic fails
// the test case instead of the whole binary, so each case reports alone.
func loadConfigString(t *testing.T, src string) (cfg *Config, err error) {
	t.Helper()
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(src), 0o644))
	require.NotPanics(t, func() {
		cfg, err = LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
	})
	return cfg, err
}

// YAML decodes a list item with nothing after the dash into a nil pointer.
// SetDefaults then dereferenced it and eRPC panicked at boot. LoadConfig must
// refuse the config and name the empty item instead.
func TestLoadConfig_EmptyListItemIsRefusedByPath(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPath string
	}{
		{"projects", "projects:\n  -\n", "projects[0]"},
		{"upstreams", "projects:\n  - id: a\n    upstreams:\n      -\n", "projects[0].upstreams[0]"},
		{"networks", "projects:\n  - id: a\n    networks:\n      -\n", "projects[0].networks[0]"},
		{"providers", "projects:\n  - id: a\n    providers:\n      -\n", "projects[0].providers[0]"},
		{"auth strategies", "projects:\n  - id: a\n    auth:\n      strategies:\n        -\n", "projects[0].auth.strategies[0]"},
		{"rate limit budgets", "rateLimiters:\n  budgets:\n    -\n", "rateLimiters.budgets[0]"},
		{"cache connectors", "database:\n  evmJsonRpcCache:\n    connectors:\n      -\n", "database.evmJsonRpcCache.connectors[0]"},
		{"cache policies", "database:\n  evmJsonRpcCache:\n    policies:\n      -\n", "database.evmJsonRpcCache.policies[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfigString(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantPath)
		})
	}
}

// A map of config objects has the same defect: a key with no value decodes
// to a nil pointer. The path skips inline fields, so it matches the YAML.
func TestLoadConfig_EmptyMapValueIsRefusedByPath(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPath string
	}{
		{
			"provider overrides",
			"projects:\n  - id: a\n    providers:\n      - vendor: alchemy\n        overrides:\n          evm:1:\n",
			"projects[0].providers[0].overrides[evm:1]",
		},
		{
			"integrity checks, an inline field",
			"projects:\n  - id: a\n    integrity:\n      checks:\n        blockHashRecompute:\n",
			"projects[0].integrity.checks[blockHashRecompute]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfigString(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantPath)
		})
	}
}

// An empty object written as {} is not an empty item.
func TestLoadConfig_EmptyObjectIsKept(t *testing.T) {
	_, err := loadConfigString(t, "projects:\n  - id: a\n    integrity:\n      checks:\n        blockHashRecompute: {}\n")
	require.NoError(t, err)
}

// A null inside a params list is a JSON-RPC value, not an empty config item.
func TestLoadConfig_NullParamIsKept(t *testing.T) {
	src := strings.Join([]string{
		"database:",
		"  evmJsonRpcCache:",
		"    connectors:",
		"      - id: memory",
		"        driver: memory",
		"    policies:",
		"      - connector: memory",
		"        network: '*'",
		"        method: eth_getLogs",
		"        params: [null, \"0x1\"]",
		"        ttl: 5s",
		"",
	}, "\n")
	cfg, err := loadConfigString(t, src)
	require.NoError(t, err)
	require.Len(t, cfg.Database.EvmJsonRpcCache.Policies, 1)
	require.Len(t, cfg.Database.EvmJsonRpcCache.Policies[0].Params, 2)
	require.Nil(t, cfg.Database.EvmJsonRpcCache.Policies[0].Params[0])
}
