package upstream

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestClassifyResponseTooLargeOutcome(t *testing.T) {
	err := common.NewErrUpstreamResponseTooLarge(1025, 1024)
	require.Equal(t, common.UpstreamOutcomeResponseTooLarge, classifyUpstreamOutcome(nil, err))
}
