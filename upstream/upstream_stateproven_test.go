package upstream

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func stateProvenUpstream(latest int64) *Upstream {
	return &Upstream{
		config: &common.UpstreamConfig{
			Id:   "u1",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		},
		logger:         &zerolog.Logger{},
		evmStatePoller: &mockEvmStatePoller{latestBlock: latest, finalizedBlock: latest - 64},
	}
}

// The proven head is telemetry — deliberately NOT a routing bound (see
// AvailbilityConfidence in common/architecture_evm.go for why: it lags the
// claimed head by the probe cadence on any fast chain, so bounding routing on
// it would refuse all tip traffic). What must still hold is its monotonicity:
// racing probe results cannot move it backwards.
func TestEvmStateProvenBlock_Monotonic(t *testing.T) {
	u := stateProvenUpstream(1000)
	u.EvmSetStateProvenBlock(950)
	u.EvmSetStateProvenBlock(940) // late/racing probe result
	assert.EqualValues(t, 950, u.EvmStateProvenBlock())
	u.EvmSetStateProvenBlock(951)
	assert.EqualValues(t, 951, u.EvmStateProvenBlock())
}
