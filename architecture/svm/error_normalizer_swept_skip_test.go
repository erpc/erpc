package svm

import (
	"testing"

	"github.com/erpc/erpc/common"
)

// Coded -32007 / -32009 land on newSweptSkipSlotError and must agree on BOTH
// axes (ExecutionException, permanent, network-retryable for one sweep).
//
// Codeless -32000 archive-gap wording is a different class — MissingData —
// covered by TestExtract_SweptArchiveGap_CodelessMissingData.
func TestExtract_SweptSkip_EntryPointsAgreeOnBothAxes(t *testing.T) {
	t.Parallel()

	const (
		wantClass     common.ErrorCode = common.ErrCodeEndpointExecutionException
		wantRetryable                  = true // sweep every upstream once...
		wantPermanent                  = true // ...but never a time-delayed re-fetch
	)

	for _, tc := range []struct {
		name string
		code int
		msg  string
	}{
		{"-32007 coded slot skipped", -32007, "Slot 12345 was skipped"},
		{"-32009 coded long-term storage", -32009, "Slot 12345 was skipped, or missing in long-term storage"},
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
			if got := wireCodeOf(t, err); got != common.JsonRpcErrorNumber(tc.code) {
				t.Fatalf("wire code = %v, want native %d", got, tc.code)
			}
		})
	}
}

// Codeless -32000 bodies that keep agave's archive-gap wording classify as
// swept MissingData (coverage), not ExecutionException skip. Same permanent +
// network-retryable axes as coded skips, different consensus hash family.
func TestExtract_SweptArchiveGap_CodelessMissingData(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"-32000 codeless long-term storage", "Slot 12345 was skipped, or missing in long-term storage"},
		{"-32000 codeless ledger jump", "Slot 12345 was skipped, or missing due to ledger jump to recent snapshot"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := extract(t, -32000, tc.msg, 200)
			if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
				t.Fatalf("got %T (%v), want ErrEndpointMissingData", err, err)
			}
			if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatal("codeless archive-gap must not be ExecutionException skip")
			}
			if !common.IsRetryableTowardNetwork(err) {
				t.Fatal("codeless archive-gap must sweep every upstream once")
			}
			if !common.IsPermanentlyMissingData(err) {
				t.Fatal("codeless archive-gap must be permanent (no time-delayed re-fetch)")
			}
			if got := wireCodeOf(t, err); got != -32000 {
				t.Fatalf("wire code = %v, want native -32000", got)
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

// No code path in the extractor may produce a coverage / skip verdict that is
// terminal toward the network. This is the invariant that stops the deleted
// authoritative-missing-data behaviour from being reintroduced under another
// name: "this provider does not have it" is never evidence that the cluster does
// not have it. The asymmetry is what settles it — answering "permanently absent"
// when another archive holds the slot can make a consumer skip a real block for
// good, while answering "not yet" only costs a retry.
//
// Driven over the whole mapped-code table, and deliberately reading the ACTUAL
// verdict rather than the table's nonRetry column. Covers both remaining
// MissingData rows and skipped-slot ExecutionExceptions (-32007/-32009).
func TestExtract_NoMissingDataVerdictIsTerminal(t *testing.T) {
	t.Parallel()

	coverageRows := 0
	sawLongTermStorageSkip := false
	for _, tc := range mappedCodeCases() {
		err := extract(t, tc.code, tc.msg, 200)
		isCoverage := common.HasErrorCode(err, common.ErrCodeEndpointMissingData) ||
			(common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) &&
				common.IsPermanentlyMissingData(err) &&
				common.IsRetryableExecutionException(err))
		if !isCoverage {
			continue
		}
		coverageRows++
		if tc.code == -32009 {
			sawLongTermStorageSkip = true
		}
		if !common.IsRetryableTowardNetwork(err) {
			t.Errorf("%s: coverage/skip verdict is terminal toward the network — one provider's gap must never answer for the whole cluster",
				tc.name)
		}
	}

	if coverageRows == 0 || !sawLongTermStorageSkip {
		t.Fatalf("table exercised %d coverage/skip rows (coded -32009 present: %v); expected MissingData + swept-skip families",
			coverageRows, sawLongTermStorageSkip)
	}
}
