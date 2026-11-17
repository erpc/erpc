package common

import (
	"testing"

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
