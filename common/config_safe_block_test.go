package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvmNetworkConfig_Validate_SafeBlock guards the trust boundary of trusted
// `safe` resolution at config-load time.
//
// `evm.safeBlock` is a promise that ONLY the named upstreams get to define what
// the `safe` tag means. A block whose source is empty or unparseable cannot
// select anybody, so honoring it would silently degrade back to forwarding the
// tag verbatim — exactly the behavior the operator wrote the block to remove.
// Load must fail loudly instead.
func TestEvmNetworkConfig_Validate_SafeBlock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		safeBlock *EvmSafeBlockConfig
		wantErr   string
	}{
		{name: "absent block keeps the network valid", safeBlock: nil},
		{name: "upstream id selector accepted", safeBlock: &EvmSafeBlockConfig{Source: "op-node-a"}},
		{name: "tag selector accepted", safeBlock: &EvmSafeBlockConfig{Source: "tier:operator"}},
		{name: "glob selector accepted", safeBlock: &EvmSafeBlockConfig{Source: "op-node-*"}},

		{
			name:      "empty source rejected",
			safeBlock: &EvmSafeBlockConfig{},
			wantErr:   "safeBlock.source is required",
		},
		{
			// A YAML value that looks present but selects nothing is the most
			// dangerous shape: it would otherwise load clean and quietly not
			// deliver the guarantee.
			name:      "whitespace-only source rejected",
			safeBlock: &EvmSafeBlockConfig{Source: "  \t "},
			wantErr:   "safeBlock.source is required",
		},
		{
			name:      "unparseable selector rejected",
			safeBlock: &EvmSafeBlockConfig{Source: "(op-node-a"},
			wantErr:   "invalid selector",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := baseValidEvmNetworkConfig()
			e.SafeBlock = tc.safeBlock

			err := e.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestEvmNetworkConfig_SafeBlockSource_NilReceiver pins the nil-receiver
// contract the resolver depends on: architecture/evm's safeBlockSourceConfigured
// probes `cfg.Evm.SafeBlockSource()` after only checking that the *network*
// config is non-nil, so a non-EVM (or evm-less) network reaches this accessor
// with a nil receiver on every request. Dropping the guard would panic there.
func TestEvmNetworkConfig_SafeBlockSource_NilReceiver(t *testing.T) {
	var noEvmBlock *EvmNetworkConfig
	assert.Equal(t, "", noEvmBlock.SafeBlockSource())
	assert.Equal(t, "", (&EvmNetworkConfig{}).SafeBlockSource())
}
