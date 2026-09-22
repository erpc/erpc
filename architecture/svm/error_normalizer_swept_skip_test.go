package svm

import (
	"testing"

	"github.com/erpc/erpc/common"
)

// Every entry point that lands on newSweptSkipMissingData must reach the SAME
// verdict on BOTH axes, and the two axes are orthogonal:
//
//	retryableTowardNetwork = true  → sweep every upstream once. "Skipped" is
//	  chain truth, but the sibling half folded into each of these messages is
//	  node-local (a post-snapshot ledger jump) or per-provider (how far an
//	  operator backfilled BigTable / Old Faithful), so another upstream may
//	  still hold the slot.
//	permanentMissingData   = true  → but no time-delayed re-fetch afterwards.
//	  Waiting cannot un-skip a slot, and cannot backfill someone else's archive.
//
// -32009 used to be terminal, which let one provider's archive gap answer for
// the whole cluster: erpc/networks.go breaks out of the sweep on a non-retryable
// verdict, so the FIRST upstream returning -32009 ended it and a slot another
// provider's archive holds was reported to the caller as permanently skipped.
//
// The expectation is declared ONCE, above the loop, because the agreement
// between these rows is itself the contract — -32007 and -32009 must not drift
// apart on either axis, and a row that diverges reddens this test by name.
func TestExtract_SweptSkip_EntryPointsAgreeOnBothAxes(t *testing.T) {
	t.Parallel()

	const (
		wantClass     common.ErrorCode = common.ErrCodeEndpointMissingData
		wantRetryable                  = true // sweep every upstream once...
		wantPermanent                  = true // ...but never a time-delayed re-fetch
	)

	for _, tc := range []struct {
		name string
		code int
		msg  string
	}{
		// Coded: agave sends the number itself.
		{"-32007 coded slot skipped", -32007, "Slot 12345 was skipped"},
		{"-32009 coded long-term storage", -32009, "Slot 12345 was skipped, or missing in long-term storage"},

		// Codeless: some vendor proxies collapse the number into agave's generic
		// -32000 bucket while forwarding the message verbatim, so the
		// message-text branches must land on the identical verdict — otherwise
		// the same physical condition routes differently per provider.
		{"-32000 codeless long-term storage", -32000, "Slot 12345 was skipped, or missing in long-term storage"},
		{"-32000 codeless ledger jump", -32000, "Slot 12345 was skipped, or missing due to ledger jump to recent snapshot"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := extract(t, tc.code, tc.msg, 200)
			if !common.HasErrorCode(err, wantClass) {
				t.Fatalf("got %T (%v), want class %s", err, err, wantClass)
			}
			if got := common.IsRetryableTowardNetwork(err); got != wantRetryable {
				t.Fatalf("retryableTowardNetwork = %v, want %v: a swept skip must try every upstream once, because the non-skip half of this message is per-node/per-provider",
					got, wantRetryable)
			}
			if got := common.IsPermanentlyMissingData(err); got != wantPermanent {
				t.Fatalf("permanentMissingData = %v, want %v: waiting cannot un-skip a slot or backfill an archive, so no time-delayed re-fetch",
					got, wantPermanent)
			}
			// The raw code still reaches the caller, so a client that wants to
			// stop on -32009 can. eRPC's routing verdict lives in the class
			// above, never in the number the client dispatches on.
			if got := wireCodeOf(t, err); got != common.JsonRpcErrorNumber(tc.code) {
				t.Fatalf("wire code = %v, want native %d", got, tc.code)
			}
		})
	}
}

// -32019 (LongTermStorageUnreachable) is the contrast case proving the taxonomy
// still distinguishes "this node's archive is DOWN" from "this node's archive
// lacks this slot" (-32009). Both are per-node facts, so both sweep — that
// symmetry is exactly why -32009 stopped being terminal — but only -32009 is a
// data-availability verdict. -32019 must therefore stay a plain server-side
// failure and must NOT carry permanentMissingData: an unreachable backend comes
// back, and suppressing the time-delayed re-fetch would turn a transient outage
// into a permanent hole.
func TestExtract_LongTermStorageUnreachable_IsServerSideNotPermanentMissingData(t *testing.T) {
	t.Parallel()
	err := extract(t, -32019, "Failed to query long-term storage; please try again", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("expected ErrEndpointServerSideException, got %T: %v", err, err)
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatal("-32019 is an archive OUTAGE, not a data-availability verdict: it must not be classified MissingData")
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32019 must stay retryable toward network: the next provider's archive may be up")
	}
	if common.IsPermanentlyMissingData(err) {
		t.Fatal("-32019 must not be permanent: the backend can recover, so the time-delayed re-fetch is worth running")
	}
}

// No code path in the extractor may produce a MissingData verdict that is
// terminal toward the network. This is the invariant that stops the deleted
// authoritative-missing-data behaviour from being reintroduced under another
// name: "this provider does not have it" is never evidence that the cluster does
// not have it. The asymmetry is what settles it — answering "permanently absent"
// when another archive holds the slot can make a consumer skip a real block for
// good, while answering "not yet" only costs a retry.
//
// Driven over the whole mapped-code table, and deliberately reading the ACTUAL
// verdict rather than the table's nonRetry column, so a future terminal
// missing-data row cannot satisfy this test by declaring itself terminal.
func TestExtract_NoMissingDataVerdictIsTerminal(t *testing.T) {
	t.Parallel()

	missingDataRows := 0
	sawLongTermStorageSkip := false
	for _, tc := range mappedCodeCases() {
		err := extract(t, tc.code, tc.msg, 200)
		if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			continue
		}
		missingDataRows++
		if tc.code == -32009 {
			sawLongTermStorageSkip = true
		}
		if !common.IsRetryableTowardNetwork(err) {
			t.Errorf("%s: MissingData verdict is terminal toward the network — one provider's gap must never answer for the whole cluster",
				tc.name)
		}
	}

	// Guard against the invariant going vacuous: if the table ever stops
	// producing MissingData verdicts, or loses the -32009 row this rule exists
	// for, the loop above would pass while asserting nothing at all.
	if missingDataRows == 0 || !sawLongTermStorageSkip {
		t.Fatalf("table exercised %d MissingData rows (coded -32009 present: %v); expected the whole missing-data family",
			missingDataRows, sawLongTermStorageSkip)
	}
}
