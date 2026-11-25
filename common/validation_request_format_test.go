package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEvmUpstreamConfig_ValidateRequestFormat(t *testing.T) {
	u := &UpstreamConfig{Endpoint: "http://rpc.localhost"}
	for _, f := range []EvmRequestFormat{"", EvmRequestFormatCanonical, EvmRequestFormatErigonLegacy} {
		e := &EvmUpstreamConfig{StatePollerInterval: Duration(1), RequestFormat: f}
		assert.NoError(t, e.Validate(u), "format %q", f)
	}
	e := &EvmUpstreamConfig{StatePollerInterval: Duration(1), RequestFormat: "erigon"}
	assert.ErrorContains(t, e.Validate(u), "requestFormat 'erigon' is invalid")
}
