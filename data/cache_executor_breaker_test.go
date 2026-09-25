package data

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/stretchr/testify/assert"
)

func TestBreakerOutcome_SemanticMissIsSuccess(t *testing.T) {
	t.Run("RecordNotFound", func(t *testing.T) {
		err := common.NewErrRecordNotFound("pk", "rk", "memory")
		assert.Equal(t, failsafe.OutcomeSuccess, breakerOutcome(err, false, false))
	})

	t.Run("RecordExpired", func(t *testing.T) {
		err := common.NewErrRecordExpired("pk", "rk", "memory", 0, 0)
		assert.Equal(t, failsafe.OutcomeSuccess, breakerOutcome(err, false, false))
	})
}
