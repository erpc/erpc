package common

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The per-request credit aggregate sums every attempt's vendor cost across
// retries/hedges/consensus, is vendor-scoped (vendorless attempts excluded),
// and keeps CreditUnitsTotal == sum(CreditUnitsByVendor).
func TestExecState_CreditAggregate(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))

	// No attempts yet → zero / nil.
	assert.Equal(t, int64(0), req.CreditUnitsTotal())
	assert.Nil(t, req.CreditUnitsByVendor())

	st := req.ExecState()
	st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "alchemy", CreditUnits: 26})
	st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "alchemy", CreditUnits: 26}) // a retry to the same vendor
	st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "quicknode", CreditUnits: 20})
	// A priced attempt to a 0-CU method contributes nothing.
	st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "drpc", CreditUnits: 0})
	// A vendorless (self-hosted) attempt is excluded from credit accounting.
	st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "", CreditUnits: 5})

	byVendor := req.CreditUnitsByVendor()
	assert.Equal(t, int64(52), byVendor["alchemy"], "retries to the same vendor accumulate")
	assert.Equal(t, int64(20), byVendor["quicknode"])
	_, hasDrpc := byVendor["drpc"]
	assert.False(t, hasDrpc, "0-cost attempts do not create a vendor entry")
	_, hasBlank := byVendor[""]
	assert.False(t, hasBlank, "vendorless attempts are excluded")

	// Total is vendor-scoped and equals the sum of the per-vendor breakdown.
	assert.Equal(t, int64(72), req.CreditUnitsTotal())
	var sum int64
	for _, v := range byVendor {
		sum += v
	}
	assert.Equal(t, req.CreditUnitsTotal(), sum, "total must equal sum of per-vendor")
}

// The aggregate is safe under concurrent RecordUpstreamAttempt (the executors
// append participant records from many goroutines) and concurrent reads.
func TestExecState_CreditAggregate_Concurrent(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	st := req.ExecState()

	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				st.RecordUpstreamAttempt(UpstreamAttempt{VendorName: "alchemy", CreditUnits: 2})
				// Concurrent readers must not race with writers.
				_ = req.CreditUnitsTotal()
				_ = req.CreditUnitsByVendor()
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine * 2)
	assert.Equal(t, want, req.CreditUnitsTotal())
	assert.Equal(t, want, req.CreditUnitsByVendor()["alchemy"])
}
