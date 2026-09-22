package common

import (
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSvmNetworkConfig_Validate(t *testing.T) {
	valid := func() *SvmNetworkConfig {
		return &SvmNetworkConfig{Cluster: "mainnet-beta"}
	}

	t.Run("minimal config with only a cluster is valid", func(t *testing.T) {
		require.NoError(t, valid().Validate())
	})

	t.Run("empty chain is legal and means solana", func(t *testing.T) {
		s := valid()
		s.Chain = ""
		require.NoError(t, s.Validate())
	})

	t.Run("explicit chain is valid", func(t *testing.T) {
		s := valid()
		s.Chain = "fogo"
		s.Cluster = "mainnet"
		require.NoError(t, s.Validate())
	})

	// Cluster is what NetworkId() and the upstream networkId are derived from;
	// without it the network resolves to "" and matches no upstream.
	t.Run("missing cluster rejected", func(t *testing.T) {
		err := (&SvmNetworkConfig{}).Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm.cluster")
	})

	t.Run("malformed cluster rejected", func(t *testing.T) {
		s := valid()
		s.Cluster = "mainnet beta" // space is not a legal id segment
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm.cluster")
	})

	t.Run("malformed chain rejected", func(t *testing.T) {
		s := valid()
		s.Chain = "fo:go" // colon would forge an extra network-id segment
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm.chain")
	})

	// An unrecognized commitment makes the injection hook a silent no-op rather
	// than an error, so every request quietly falls back to the vendor default.
	for _, c := range []string{"processed", "confirmed", "finalized", "Finalized", "", "CONFIRMED"} {
		t.Run("commitment "+c+" accepted", func(t *testing.T) {
			s := valid()
			s.Commitment = c
			require.NoError(t, s.Validate())
		})
	}

	for _, c := range []string{"finalised", "safe", "latest", "root"} {
		t.Run("commitment "+c+" rejected", func(t *testing.T) {
			s := valid()
			s.Commitment = c
			err := s.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "network.*.svm.commitment")
		})
	}

	t.Run("negative statePollerDebounce rejected", func(t *testing.T) {
		s := valid()
		s.StatePollerDebounce = Duration(-1)
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm.statePollerDebounce")
	})

	t.Run("negative maxFinalizedSlotLag rejected", func(t *testing.T) {
		s := valid()
		s.MaxFinalizedSlotLag = int64Ptr(-5)
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm.maxFinalizedSlotLag")
	})

	// nil is "unset" and 0 is the documented disable switch — neither is an error.
	t.Run("nil and zero maxFinalizedSlotLag accepted", func(t *testing.T) {
		s := valid()
		s.MaxFinalizedSlotLag = nil
		require.NoError(t, s.Validate())
		s.MaxFinalizedSlotLag = int64Ptr(0)
		require.NoError(t, s.Validate())
	})
}

func TestNetworkConfig_Validate_Svm(t *testing.T) {
	cfg := &Config{}

	t.Run("architecture=svm without an svm block rejected", func(t *testing.T) {
		err := (&NetworkConfig{Architecture: ArchitectureSvm}).Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.svm is required")
	})

	t.Run("svm block errors surface through NetworkConfig.Validate", func(t *testing.T) {
		n := &NetworkConfig{
			Architecture: ArchitectureSvm,
			Svm:          &SvmNetworkConfig{Cluster: "mainnet-beta", Commitment: "finalised"},
		}
		err := n.Validate(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "commitment")
	})

	// EVM invariance: the new svm branch must not disturb the evm path.
	t.Run("evm network still validates unchanged", func(t *testing.T) {
		n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: baseValidEvmNetworkConfig()}
		require.NoError(t, n.Validate(cfg))
	})
}

func TestSvmUpstreamConfig_Validate(t *testing.T) {
	svmUps := &UpstreamConfig{Id: "u1", Type: UpstreamTypeSvm, Endpoint: "http://localhost:8899"}

	t.Run("cluster present is valid", func(t *testing.T) {
		require.NoError(t, (&SvmUpstreamConfig{Cluster: "mainnet-beta"}).Validate(svmUps))
	})

	// Previously this only failed at bootstrap, long after startup reported OK.
	t.Run("missing cluster rejected on an svm upstream", func(t *testing.T) {
		err := (&SvmUpstreamConfig{}).Validate(svmUps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.*.svm.cluster")
	})

	// A stray svm block inherited from upstreamDefaults onto an evm upstream in a
	// mixed project is inert, not a config error.
	t.Run("missing cluster tolerated on a non-svm upstream", func(t *testing.T) {
		evmUps := &UpstreamConfig{Id: "u2", Type: UpstreamTypeEvm, Endpoint: "http://localhost:8545"}
		require.NoError(t, (&SvmUpstreamConfig{}).Validate(evmUps))
	})

	t.Run("malformed chain rejected", func(t *testing.T) {
		err := (&SvmUpstreamConfig{Chain: "fo go", Cluster: "mainnet"}).Validate(svmUps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.*.svm.chain")
	})

	t.Run("malformed cluster rejected", func(t *testing.T) {
		err := (&SvmUpstreamConfig{Cluster: "main:net"}).Validate(svmUps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.*.svm.cluster")
	})
}

func TestValidateSvmUpstreamNetworkPairing(t *testing.T) {
	ups := func(chain, cluster string) *UpstreamConfig {
		return &UpstreamConfig{Id: "u1", Type: UpstreamTypeSvm, Svm: &SvmUpstreamConfig{Chain: chain, Cluster: cluster}}
	}
	ntw := func(chain, cluster string) *NetworkConfig {
		return &NetworkConfig{Architecture: ArchitectureSvm, Svm: &SvmNetworkConfig{Chain: chain, Cluster: cluster}}
	}

	t.Run("matching chain and cluster passes", func(t *testing.T) {
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("fogo", "mainnet")},
			[]*NetworkConfig{ntw("fogo", "mainnet")},
		))
	})

	// Empty chain resolves to solana on both sides, so these must pair up.
	t.Run("empty chain pairs with explicit solana", func(t *testing.T) {
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("", "mainnet-beta")},
			[]*NetworkConfig{ntw("solana", "mainnet-beta")},
		))
	})

	t.Run("chain mismatch on a declared cluster rejected", func(t *testing.T) {
		err := validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("fogo", "mainnet-beta")},
			[]*NetworkConfig{ntw("", "mainnet-beta")},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.*.svm.chain")
	})

	// A sibling network on the right chain means the upstream has a home.
	t.Run("passes when some network matches exactly", func(t *testing.T) {
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("fogo", "mainnet-beta")},
			[]*NetworkConfig{ntw("", "mainnet-beta"), ntw("fogo", "mainnet-beta")},
		))
	})

	// Lazy network creation: no network declares that cluster, so we stay quiet.
	t.Run("undeclared cluster is not an error", func(t *testing.T) {
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("fogo", "mainnet")},
			[]*NetworkConfig{ntw("", "mainnet-beta")},
		))
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{ups("fogo", "mainnet")},
			nil,
		))
	})

	t.Run("evm upstreams ignored", func(t *testing.T) {
		require.NoError(t, validateSvmUpstreamNetworkPairing(
			[]*UpstreamConfig{{Id: "e1", Type: UpstreamTypeEvm, Evm: &EvmUpstreamConfig{ChainId: 1}}},
			[]*NetworkConfig{ntw("", "mainnet-beta")},
		))
	})
}

// The SVM cache config was never validated, so a policy naming a connector that
// does not exist passed startup and then silently disabled caching at runtime
// (erpc/init.go downgrades the constructor error to a warning).
func TestDatabaseConfig_Validate_SvmJsonRpcCache(t *testing.T) {
	newCache := func(connectorRef string) *CacheConfig {
		return &CacheConfig{
			Connectors: []*ConnectorConfig{
				{Id: "mem", Driver: DriverMemory, Memory: &MemoryConnectorConfig{MaxItems: 10, MaxTotalSize: "1MB"}},
			},
			Policies: []*CachePolicyConfig{
				{Network: "svm:mainnet-beta", Method: "*", Connector: connectorRef},
			},
		}
	}

	t.Run("valid svm cache passes", func(t *testing.T) {
		require.NoError(t, (&DatabaseConfig{SvmJsonRpcCache: newCache("mem")}).Validate())
	})

	t.Run("unknown connector reference rejected", func(t *testing.T) {
		err := (&DatabaseConfig{SvmJsonRpcCache: newCache("does-not-exist")}).Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cache.*.policies.*.connector")
	})

	t.Run("missing policy method rejected", func(t *testing.T) {
		c := newCache("mem")
		c.Policies[0].Method = ""
		err := (&DatabaseConfig{SvmJsonRpcCache: c}).Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cache.*.policies.*.method")
	})

	t.Run("nil svm cache is fine", func(t *testing.T) {
		require.NoError(t, (&DatabaseConfig{}).Validate())
	})
}

// TestLoadConfig_SvmEndToEnd runs a realistic SVM config through the real
// LoadConfig path (decode -> SetDefaults -> Validate). It guards both directions
// of the new SVM validation: a well-formed config must still load, and each
// class of defect must be caught at startup instead of at request time.
func TestLoadConfig_SvmEndToEnd(t *testing.T) {
	load := func(t *testing.T, yaml string) (*Config, error) {
		t.Helper()
		fs := afero.NewMemMapFs()
		require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(yaml), 0644))
		return LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
	}

	// Mirrors erpc.svm.example.yaml: networkDefaults.svm + two clusters, each
	// with a matching upstream, plus an svmJsonRpcCache block.
	const good = `
logLevel: warn
server:
  httpHostV4: 127.0.0.1
  httpPort: 4000
database:
  svmJsonRpcCache:
    connectors:
      - id: mem
        driver: memory
        memory:
          maxItems: 1000
          maxTotalSize: 1MB
    policies:
      - connector: mem
        network: "svm:*"
        method: "*"
        finality: finalized
projects:
  - id: main
    networkDefaults:
      svm:
        commitment: confirmed
        statePollerDebounce: 500ms
    networks:
      - architecture: svm
        svm:
          cluster: mainnet-beta
      - architecture: svm
        svm:
          cluster: testnet
    upstreams:
      - id: mainnet
        type: svm
        endpoint: https://api.mainnet-beta.solana.com
        svm:
          cluster: mainnet-beta
      - id: testnet
        type: svm
        endpoint: https://api.testnet.solana.com
        svm:
          cluster: testnet
`

	t.Run("realistic svm config loads and validates", func(t *testing.T) {
		cfg, err := load(t, good)
		require.NoError(t, err)
		require.Len(t, cfg.Projects[0].Networks, 2)

		n := cfg.Projects[0].Networks[0]
		assert.Equal(t, ArchitectureSvm, n.Architecture)
		assert.Equal(t, "confirmed", n.Svm.Commitment, "networkDefaults.svm must reach the network")
		require.NotNil(t, n.Svm.MaxFinalizedSlotLag)
		assert.Equal(t, MaxShredInsertSlotLagThreshold, *n.Svm.MaxFinalizedSlotLag)
	})

	// The disable switches, exercised through YAML rather than struct literals —
	// this is the shape an operator actually writes.
	t.Run("yaml disable switches survive to the loaded config", func(t *testing.T) {
		yaml := strings.Replace(good,
			"        statePollerDebounce: 500ms",
			"        statePollerDebounce: 500ms\n        maxFinalizedSlotLag: 0\n        enforceBlockAvailability: false",
			1)
		cfg, err := load(t, yaml)
		require.NoError(t, err)

		n := cfg.Projects[0].Networks[0]
		require.NotNil(t, n.Svm.MaxFinalizedSlotLag)
		assert.Equal(t, int64(0), *n.Svm.MaxFinalizedSlotLag, "maxFinalizedSlotLag: 0 must disable the filter")
		require.NotNil(t, n.Svm.EnforceBlockAvailability)
		assert.False(t, *n.Svm.EnforceBlockAvailability, "enforceBlockAvailability: false must not be dropped")
	})

	for _, tc := range []struct {
		name, old, new, wantErr string
	}{
		{
			name:    "invalid commitment",
			old:     "        commitment: confirmed",
			new:     "        commitment: finalised",
			wantErr: "svm.commitment",
		},
		{
			name:    "negative maxFinalizedSlotLag",
			old:     "        statePollerDebounce: 500ms",
			new:     "        statePollerDebounce: 500ms\n        maxFinalizedSlotLag: -1",
			wantErr: "svm.maxFinalizedSlotLag",
		},
		{
			// Anchored on the endpoint line: the network and upstream svm blocks
			// are textually identical, so a bare "svm:\n cluster: testnet" would
			// hit the network first and assert the wrong error.
			name:    "upstream missing cluster",
			old:     "endpoint: https://api.testnet.solana.com\n        svm:\n          cluster: testnet\n",
			new:     "endpoint: https://api.testnet.solana.com\n        svm: {}\n",
			wantErr: "upstream.*.svm.cluster",
		},
		{
			name:    "cache policy references unknown connector",
			old:     "      - connector: mem",
			new:     "      - connector: nope",
			wantErr: "cache.*.policies.*.connector",
		},
		{
			name:    "upstream chain does not match the network chain",
			old:     "endpoint: https://api.mainnet-beta.solana.com\n        svm:\n          cluster: mainnet-beta\n",
			new:     "endpoint: https://api.mainnet-beta.solana.com\n        svm:\n          chain: fogo\n          cluster: mainnet-beta\n",
			wantErr: "upstream.*.svm.chain",
		},
	} {
		t.Run(tc.name+" is rejected at load time", func(t *testing.T) {
			yaml := strings.Replace(good, tc.old, tc.new, 1)
			require.NotEqual(t, good, yaml, "test fixture did not apply — the anchor string drifted")

			_, err := load(t, yaml)
			require.Error(t, err, "this must fail at startup, not asynchronously at request time")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
