package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func missingData(code int, permanent bool) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorMissingData, "missing", nil, nil),
		nil,
	)
	if permanent {
		err.(*common.ErrEndpointMissingData).WithPermanentMissingData(true)
	}
	return err
}

func TestErrorToConsensusHash_PermanentAndTransientMissingDataDiffer(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "jsonrpc:-32014", errorToConsensusHash(missingData(-32004, false)))
	assert.Equal(t, "jsonrpc:-32014:permanent", errorToConsensusHash(missingData(-32007, true)))
	assert.Equal(t, errorToConsensusHash(missingData(-32007, true)), errorToConsensusHash(missingData(-32009, true)))
}
