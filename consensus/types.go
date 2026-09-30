package consensus

import (
	"github.com/erpc/erpc/common"
)

// slotResult is the (response, error) pair produced by one consensus
// participant slot. The analyzer picks a winner across all slots.
//
// Holding both fields lets the analyzer carry a "response-and-error"
// pair when a JSON-RPC error sits next to a parsed response body (e.g.
// execution exception with a structured payload).
type slotResult struct {
	Result *common.NormalizedResponse
	Error  error
	// Policy is the name of the acceptance grade the round resolved under,
	// empty when no acceptance policy is configured. Carried on the result
	// so the short-circuit guard can prefer a stricter grade that is still
	// achievable, and so the served grade can be reported to the caller.
	Policy string
}
