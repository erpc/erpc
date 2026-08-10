package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func networkCfgWithDefinitions(defs map[string]*CacheMethodConfig) *NetworkConfig {
	return &NetworkConfig{
		Architecture: ArchitectureEvm,
		Evm: &EvmNetworkConfig{
			ChainId:                 123,
			MarkEmptyAsErrorMethods: DefaultMarkEmptyAsErrorMethods(),
		},
		Methods: &MethodsConfig{Definitions: defs},
	}
}

// TestResolveEmptyResultBehavior covers the whole resolution: the two shipped
// lists as defaults, per-method overrides on top, and the open-set fallthrough.
func TestResolveEmptyResultBehavior(t *testing.T) {
	accept := DefaultEmptyResultAccept()

	cases := []struct {
		name         string
		cfg          *NetworkConfig
		method       string
		acceptList   []string
		wantBehavior EmptyResultBehavior
		wantSource   EmptyResultSource
	}{
		// Defaults — accept list.
		{"acceptList_getLogs", networkCfgWithDefinitions(nil), "eth_getLogs", accept,
			EmptyResultBehaviorAccept, EmptyResultSourceAcceptList},
		{"acceptList_getStorageAt", networkCfgWithDefinitions(nil), "eth_getStorageAt", accept,
			EmptyResultBehaviorAccept, EmptyResultSourceAcceptList},

		// Defaults — mark-empty-as-error list (case-insensitive, mirroring the
		// EVM post-forward hook that owns this list).
		{"errorList_getBlockByNumber", networkCfgWithDefinitions(nil), "eth_getBlockByNumber", accept,
			EmptyResultBehaviorError, EmptyResultSourceErrorList},
		{"errorList_caseInsensitive", networkCfgWithDefinitions(nil), "eth_getblockbynumber", accept,
			EmptyResultBehaviorError, EmptyResultSourceErrorList},

		// Fallthrough — the open-set default path.
		{"fallthrough_unknownMethod", networkCfgWithDefinitions(nil), "myrollup_getSomething", accept,
			EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},
		{"fallthrough_getBlockReceipts", networkCfgWithDefinitions(nil), "eth_getBlockReceipts", accept,
			EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},
		{"fallthrough_getTransactionReceipt", networkCfgWithDefinitions(nil), "eth_getTransactionReceipt", accept,
			EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},
		{"fallthrough_nilConfig", nil, "myrollup_getSomething", accept,
			EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},
		{"fallthrough_nilConfigNilList", nil, "eth_getLogs", nil,
			EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},

		// Overrides beat both lists and the fallthrough.
		{"override_acceptOnUnknown", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"myrollup_getSomething": {EmptyResult: EmptyResultBehaviorAccept},
		}), "myrollup_getSomething", accept, EmptyResultBehaviorAccept, EmptyResultSourceOverride},
		{"override_errorOnUnknown", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"myrollup_getSomething": {EmptyResult: EmptyResultBehaviorError},
		}), "myrollup_getSomething", accept, EmptyResultBehaviorError, EmptyResultSourceOverride},
		{"override_errorOnAcceptListMethod", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"eth_getLogs": {EmptyResult: EmptyResultBehaviorError},
		}), "eth_getLogs", accept, EmptyResultBehaviorError, EmptyResultSourceOverride},
		{"override_acceptOnErrorListMethod", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"eth_getBlockByNumber": {EmptyResult: EmptyResultBehaviorAccept},
		}), "eth_getBlockByNumber", accept, EmptyResultBehaviorAccept, EmptyResultSourceOverride},

		// `default` is not an override — it defers, exactly like unset.
		{"explicitDefault_defersToAcceptList", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"eth_getLogs": {EmptyResult: EmptyResultBehaviorDefault},
		}), "eth_getLogs", accept, EmptyResultBehaviorAccept, EmptyResultSourceAcceptList},
		{"explicitDefault_defersToFallthrough", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"myrollup_getSomething": {EmptyResult: EmptyResultBehaviorDefault},
		}), "myrollup_getSomething", accept, EmptyResultBehaviorDefault, EmptyResultSourceFallthrough},

		// A definition without the field (the shape every default cache-method
		// definition has) must not change anything.
		{"unsetDefinition_defersToAcceptList", networkCfgWithDefinitions(map[string]*CacheMethodConfig{
			"eth_getLogs": {Finalized: true},
		}), "eth_getLogs", accept, EmptyResultBehaviorAccept, EmptyResultSourceAcceptList},

		// A method in BOTH lists resolves to accept, matching the pre-existing
		// network-retry behaviour (that layer only ever consulted the accept list).
		{"bothLists_acceptWins", networkCfgWithDefinitions(nil), "eth_getBlockByNumber",
			append(DefaultEmptyResultAccept(), "eth_getBlockByNumber"),
			EmptyResultBehaviorAccept, EmptyResultSourceAcceptList},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			behavior, source := ResolveEmptyResultBehavior(tc.cfg, tc.method, tc.acceptList)
			assert.Equal(t, tc.wantBehavior, behavior, "behavior for %s", tc.method)
			assert.Equal(t, tc.wantSource, source, "source for %s", tc.method)
		})
	}
}

// Every method in the shipped defaults must resolve to a definite behaviour from
// its own list — no default method may silently land on the fallthrough.
func TestResolveEmptyResultBehavior_ShippedListsAreExhaustive(t *testing.T) {
	cfg := networkCfgWithDefinitions(nil)
	accept := DefaultEmptyResultAccept()

	for _, m := range accept {
		behavior, source := ResolveEmptyResultBehavior(cfg, m, accept)
		assert.Equal(t, EmptyResultBehaviorAccept, behavior, "method %s", m)
		assert.Equal(t, EmptyResultSourceAcceptList, source, "method %s", m)
	}
	for _, m := range DefaultMarkEmptyAsErrorMethods() {
		behavior, source := ResolveEmptyResultBehavior(cfg, m, accept)
		assert.Equal(t, EmptyResultBehaviorError, behavior, "method %s", m)
		assert.Equal(t, EmptyResultSourceErrorList, source, "method %s", m)
	}
}

// The default lists must stay disjoint: a method in both would resolve to accept
// at the network-retry layer while the EVM post-forward hook converted its empty
// into a missing-data error — two layers disagreeing about the same method.
func TestDefaultEmptyResultListsAreDisjoint(t *testing.T) {
	inAccept := map[string]bool{}
	for _, m := range DefaultEmptyResultAccept() {
		inAccept[m] = true
	}
	for _, m := range DefaultMarkEmptyAsErrorMethods() {
		assert.False(t, inAccept[m], "method %s is in both default empty-result lists", m)
	}
}

// The emptyResult field's value set is closed, so a typo must fail at config load
// rather than silently resolving to the default at runtime.
func TestMethodsConfig_Validate_EmptyResult(t *testing.T) {
	for _, v := range []EmptyResultBehavior{"", EmptyResultBehaviorDefault, EmptyResultBehaviorAccept, EmptyResultBehaviorError} {
		m := &MethodsConfig{Definitions: map[string]*CacheMethodConfig{
			"eth_getLogs": {EmptyResult: v},
		}}
		require.NoError(t, m.Validate(), "value %q must be accepted", v)
	}

	m := &MethodsConfig{Definitions: map[string]*CacheMethodConfig{
		"eth_getLogs": {EmptyResult: EmptyResultBehavior("Accept")},
	}}
	err := m.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "emptyResult must be one of")

	var nilCfg *MethodsConfig
	assert.NoError(t, nilCfg.Validate())
}

func loadConfigFromYaml(t *testing.T, src string) (*Config, error) {
	t.Helper()
	fs := afero.NewMemMapFs()
	tmp, err := afero.TempFile(fs, "", "empty-result.yaml")
	require.NoError(t, err)
	_, err = tmp.WriteString(src)
	require.NoError(t, err)
	return LoadConfig(fs, tmp.Name(), &DefaultOptions{})
}

// Pins the public YAML key. The override rides on the existing per-method
// definitions map, so it survives SetDefaults' merge of the shipped cache-method
// definitions and the shipped default lists keep deciding every other method.
func TestLoadConfig_MethodEmptyResultOverride(t *testing.T) {
	cfg, err := loadConfigFromYaml(t, `
logLevel: error
projects:
  - id: test
    upstreams:
      - id: up1
        endpoint: https://rpc.example/
        evm: { chainId: 1 }
    networks:
      - architecture: evm
        evm: { chainId: 1 }
        methods:
          preserveDefaultMethods: true
          definitions:
            myrollup_getSomething:
              emptyResult: accept
`)
	require.NoError(t, err)
	require.Len(t, cfg.Projects, 1)
	require.Len(t, cfg.Projects[0].Networks, 1)
	nwCfg := cfg.Projects[0].Networks[0]

	behavior, source := ResolveEmptyResultBehavior(nwCfg, "myrollup_getSomething", DefaultEmptyResultAccept())
	assert.Equal(t, EmptyResultBehaviorAccept, behavior)
	assert.Equal(t, EmptyResultSourceOverride, source)

	// Everything else keeps resolving from the shipped defaults.
	behavior, source = ResolveEmptyResultBehavior(nwCfg, "eth_getLogs", DefaultEmptyResultAccept())
	assert.Equal(t, EmptyResultBehaviorAccept, behavior)
	assert.Equal(t, EmptyResultSourceAcceptList, source)

	behavior, source = ResolveEmptyResultBehavior(nwCfg, "eth_getBlockByNumber", DefaultEmptyResultAccept())
	assert.Equal(t, EmptyResultBehaviorError, behavior)
	assert.Equal(t, EmptyResultSourceErrorList, source)

	behavior, source = ResolveEmptyResultBehavior(nwCfg, "myrollup_getSomethingElse", DefaultEmptyResultAccept())
	assert.Equal(t, EmptyResultBehaviorDefault, behavior)
	assert.Equal(t, EmptyResultSourceFallthrough, source)
}

func TestLoadConfig_MethodEmptyResultInvalidValueRejected(t *testing.T) {
	_, err := loadConfigFromYaml(t, `
logLevel: error
projects:
  - id: test
    upstreams:
      - id: up1
        endpoint: https://rpc.example/
        evm: { chainId: 1 }
    networks:
      - architecture: evm
        evm: { chainId: 1 }
        methods:
          definitions:
            myrollup_getSomething:
              emptyResult: retry
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "emptyResult must be one of")
}
