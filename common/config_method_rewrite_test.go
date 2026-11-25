package common

import (
	"strings"
	"testing"

	"github.com/spf13/afero"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// methodRewrite survives every path that builds an upstream config: the
// canonical YAML shape, the legacy single-object failsafe shape, and Copy()
// (provider overrides). A dropped map would silently send the client's
// method unrenamed.
func TestUpstreamConfig_MethodRewrite_YamlAndCopy(t *testing.T) {
	want := map[string]string{"custom_sendMevProtected": "eth_sendRawTransaction"}

	for name, src := range map[string]string{
		"canonical": `
id: relay
endpoint: https://relay.example
failsafe:
  - matchMethod: "*"
methodRewrite:
  custom_sendMevProtected: eth_sendRawTransaction
`,
		"legacy failsafe object": `
id: relay
endpoint: https://relay.example
failsafe:
  matchMethod: "*"
methodRewrite:
  custom_sendMevProtected: eth_sendRawTransaction
`,
	} {
		t.Run(name, func(t *testing.T) {
			var u UpstreamConfig
			require.NoError(t, yaml.Unmarshal([]byte(src), &u))
			assert.Equal(t, want, u.MethodRewrite)

			c := u.Copy()
			assert.Equal(t, want, c.MethodRewrite)
			c.MethodRewrite["custom_sendMevProtected"] = "changed"
			assert.Equal(t, "eth_sendRawTransaction", u.MethodRewrite["custom_sendMevProtected"], "Copy must not share the map")
		})
	}
}

func TestUpstreamConfig_WireMethod(t *testing.T) {
	u := &UpstreamConfig{MethodRewrite: map[string]string{"custom_send": "eth_sendRawTransaction"}}
	assert.Equal(t, "eth_sendRawTransaction", u.WireMethod("custom_send"))
	assert.Equal(t, "eth_chainId", u.WireMethod("eth_chainId"), "unmapped method goes out unchanged")
	assert.Equal(t, "custom_send", (*UpstreamConfig)(nil).WireMethod("custom_send"))
	assert.Equal(t, "custom_send", (&UpstreamConfig{}).WireMethod("custom_send"))
}

// methodRewriteExampleYaml is worked example #7 of
// docs/pages/config/projects/upstreams.mdx, verbatim.
const methodRewriteExampleYaml = `projects:
  - id: main
    networkDefaults:
      failsafe:
        # Each network failsafe entry takes its defaults from the FIRST entry
        # here that covers it: keep the custom send's entry, without hedge,
        # before "*" so a slow relay never pushes the tx to the public node
        - matchMethod: custom_sendMevProtected
          retry: { maxAttempts: 3 }
        - matchMethod: "*"
          retry: { maxAttempts: 3 }
          hedge: { delay: { quantile: 0.7 }, maxCount: 1 }
    upstreams:
      - id: regular
        endpoint: https://node.example/rpc
        # Lets the regular node take the custom method as the fallback
        methodRewrite:
          custom_sendMevProtected: eth_sendRawTransaction
      - id: mev-relay-a
        endpoint: https://relay-a.example
        evm: { chainId: 1 }
        tags: [mev]
        allowMethods: [custom_sendMevProtected]
        methodRewrite:
          custom_sendMevProtected: eth_sendRawTransaction
      - id: mev-relay-b
        # Each relay keeps its own URL path
        endpoint: https://relay-b.example/v1/jsonrpc
        evm: { chainId: 1 }
        tags: [mev]
        allowMethods: [custom_sendMevProtected]
        methodRewrite:
          custom_sendMevProtected: eth_sendRawTransaction
    networks:
      - architecture: evm
        evm: { chainId: 1 }
        selectionPolicy:
          # One ranking per method, so the custom method gets its own order
          evalScope: network-method
          evalFunc: |
            (upstreams, ctx) =>
              methodMatches('custom_sendMevProtected')
                ? upstreams.byTag('mev').sortByScore(PREFER_FASTEST).concat(upstreams.excludeTag('mev'))
                : upstreams.excludeTag('mev').sortByScore(PREFER_FASTEST)
`

// hedgeOf returns the hedge policy of the network failsafe entry whose
// matchMethod is method, after LoadConfig (defaults applied).
func hedgeOf(t *testing.T, yamlSrc, method string) *HedgePolicyConfig {
	t.Helper()
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(yamlSrc), 0o644))
	cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
	require.NoError(t, err)
	for _, f := range cfg.Projects[0].Networks[0].Failsafe {
		if f.MatchMethod == method {
			return f.Hedge
		}
	}
	t.Fatalf("no network failsafe entry for %s", method)
	return nil
}

// A slow relay must never make the network hedge a renamed private send to
// the public fallback node. The docs example keeps it off even though its
// networkDefaults hedge "*"; the last case pins why the docs warn that a
// network's own `hedge: null` is not enough.
func TestMethodRewriteExample_CustomSendNeverHedges(t *testing.T) {
	t.Run("docs example", func(t *testing.T) {
		assert.Nil(t, hedgeOf(t, methodRewriteExampleYaml, "custom_sendMevProtected"))
		assert.NotNil(t, hedgeOf(t, methodRewriteExampleYaml, "*"), "other methods keep their hedge")
	})

	t.Run("network's own entry inherits the custom default", func(t *testing.T) {
		src := strings.Replace(methodRewriteExampleYaml, "      - architecture: evm\n        evm: { chainId: 1 }\n",
			"      - architecture: evm\n        evm: { chainId: 1 }\n        failsafe:\n          - matchMethod: custom_sendMevProtected\n            retry: { maxAttempts: 2 }\n          - matchMethod: \"*\"\n", 1)
		require.NotEqual(t, methodRewriteExampleYaml, src)
		assert.Nil(t, hedgeOf(t, src, "custom_sendMevProtected"))
	})

	t.Run("hedge null under a defaults hedge is filled in", func(t *testing.T) {
		src := `projects:
  - id: main
    networkDefaults:
      failsafe:
        - matchMethod: "*"
          hedge: { maxCount: 2 }
    upstreams:
      - id: regular
        endpoint: https://node.example/rpc
    networks:
      - architecture: evm
        evm: { chainId: 1 }
        failsafe:
          - matchMethod: custom_sendMevProtected
            hedge: null
          - matchMethod: "*"
`
		h := hedgeOf(t, src, "custom_sendMevProtected")
		require.NotNil(t, h)
		assert.Equal(t, 2, h.MaxCount)
	})
}
