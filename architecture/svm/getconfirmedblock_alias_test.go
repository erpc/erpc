package svm

import (
	"context"
	"reflect"
	"testing"

	"github.com/erpc/erpc/common"
)

// getBlockAliases are the two method names handler.go dispatches to the SAME
// pre-forward path (`case "getBlock", "getConfirmedBlock":` →
// networkPreForward_getBlock). getConfirmedBlock is the deprecated alias of
// getBlock with an identical signature (slot first, options object second), so
// every classification and mutation the pipeline applies to one MUST apply
// identically to the other.
//
// The alias is routed by a switch but recognized by lookup TABLES elsewhere
// (slotPinnedMethods, commitmentOptionsIndex), and a table entry is easy to
// drop while the switch keeps working — which is exactly how the alias once
// ended up with the availability guard but no immutable-cache classification
// and no commitment injection. The tests below are written as one table per
// behaviour with a SINGLE expectation applied to both names, so a divergence
// between the two is not expressible: dropping either table entry reddens a row
// rather than silently degrading the deprecated name.
var getBlockAliases = []string{"getBlock", "getConfirmedBlock"}

// TestGetConfirmedBlockAlias_FinalityParity pins that GetFinality classifies
// both names identically across the request shapes that decide cacheability.
//
// The load-bearing rows are the Finalized ones: with getConfirmedBlock absent
// from slotPinnedMethods, GetFinality's step 4 treats it as a moving-head read
// and returns Realtime, so a finalized historical read through the old name can
// never match an immutable cache policy. Every row here would also catch the
// inverse error (promoting a below-finalized read to Finalized).
func TestGetConfirmedBlockAlias_FinalityParity(t *testing.T) {
	t.Parallel()

	confirmedDefault := func() *common.NetworkConfig {
		return &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
		}
	}
	finalizedDefault := func() *common.NetworkConfig {
		return &common.NetworkConfig{
			Architecture: common.ArchitectureSvm,
			Svm:          &common.SvmNetworkConfig{Commitment: "finalized"},
		}
	}
	noDefault := func() *common.NetworkConfig {
		return &common.NetworkConfig{Architecture: common.ArchitectureSvm}
	}

	cases := []struct {
		name   string
		cfg    func() *common.NetworkConfig
		params string
		want   common.DataFinalityState
	}{
		// Commitment finalized: slot-pinned + rooted → immutable, cacheable
		// forever. This is the row the missing slotPinnedMethods entry broke.
		{
			name:   "explicit finalized is immutable",
			cfg:    confirmedDefault,
			params: `[100, {"commitment":"finalized"}]`,
			want:   common.DataFinalityStateFinalized,
		},
		// Commitment confirmed: pinned to a slot but not yet rooted, so a fork
		// switch can still replace it → re-org aware, never Finalized.
		{
			name:   "explicit confirmed is fork-droppable",
			cfg:    confirmedDefault,
			params: `[100, {"commitment":"confirmed"}]`,
			want:   common.DataFinalityStateUnfinalized,
		},
		// Commitment absent: the effective commitment is the injected network
		// default, so the classification follows that default both ways.
		{
			name:   "commitment absent, finalized network default",
			cfg:    finalizedDefault,
			params: `[100]`,
			want:   common.DataFinalityStateFinalized,
		},
		{
			name:   "commitment absent, confirmed network default",
			cfg:    confirmedDefault,
			params: `[100]`,
			want:   common.DataFinalityStateUnfinalized,
		},
		{
			name:   "commitment absent, no network default",
			cfg:    noDefault,
			params: `[100]`,
			want:   common.DataFinalityStateUnfinalized,
		},
		// An empty options object still receives the injected default, so it is
		// promotable just like the bare-slot form.
		{
			name:   "empty options object takes the finalized default",
			cfg:    finalizedDefault,
			params: `[100, {}]`,
			want:   common.DataFinalityStateFinalized,
		},
		// Explicit commitment beats the network default in both directions.
		{
			name:   "explicit confirmed beats finalized default",
			cfg:    finalizedDefault,
			params: `[100, {"commitment":"confirmed"}]`,
			want:   common.DataFinalityStateUnfinalized,
		},
		{
			name:   "explicit finalized beats confirmed default",
			cfg:    confirmedDefault,
			params: `[100, {"commitment":"finalized"}]`,
			want:   common.DataFinalityStateFinalized,
		},
		// Legacy encoding-string form: the options slot is occupied by a string
		// so injection must skip, the default never reaches the upstream, and
		// the response must NOT be trusted as finalized.
		{
			name:   "legacy encoding form is not promoted by the default",
			cfg:    finalizedDefault,
			params: `[100, "base64"]`,
			want:   common.DataFinalityStateUnfinalized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, method := range getBlockAliases {
				net := &fakeNetwork{cfg: tc.cfg()}
				got := GetFinality(context.Background(), net, newReq(method, tc.params), nil)
				if got != tc.want {
					t.Errorf("%s%s: got %v, want %v", method, tc.params, got, tc.want)
				}
			}
		})
	}
}

// TestGetConfirmedBlockAlias_CommitmentInjectionParity pins that commitment
// injection rewrites both names into the exact same params — same options-object
// POSITION and same value — which is what commitmentOptionsIndex controls.
//
// Asserting the whole params slice (rather than just "commitment is somewhere")
// is deliberate: the alias shares getBlock's signature, so the options object
// belongs at index 1 and the slot must stay at index 0. With the alias missing
// from commitmentOptionsIndex, resolveCommitment reports commitmentSkip and the
// params come back unmutated — no default reaches the upstream, and the two
// names produce different request bodies (and therefore different cache keys)
// for the same logical read.
func TestGetConfirmedBlockAlias_CommitmentInjectionParity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params string
		want   []interface{}
	}{
		// The shape the contract calls out: only the slot argument present, so
		// the options object is appended at index 1 and must not displace it.
		{
			name:   "bare slot appends options at index 1",
			params: `[100]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"commitment": "confirmed"},
			},
		},
		// Existing config object at index 1 → commitment merged in, caller's
		// other fields preserved.
		{
			name:   "existing options object receives commitment",
			params: `[100, {"encoding":"json"}]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"encoding": "json", "commitment": "confirmed"},
			},
		},
		// A caller-supplied commitment is authoritative and never rewritten to
		// the network default.
		{
			name:   "caller commitment is not overwritten",
			params: `[100, {"commitment":"finalized"}]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"commitment": "finalized"},
			},
		},
		// Legacy getBlock(slot, "encoding"): the options slot holds a string, so
		// injection must leave the request alone rather than append an invalid
		// third param.
		{
			name:   "legacy encoding form left untouched",
			params: `[100, "base64"]`,
			want:   []interface{}{float64(100), "base64"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, method := range getBlockAliases {
				net := &fakeNetwork{cfg: &common.NetworkConfig{
					Architecture: common.ArchitectureSvm,
					Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
				}}
				req := newReq(method, tc.params)
				if _, _, err := networkPreForward_injectCommitment(context.Background(), net, req); err != nil {
					t.Fatalf("%s: unexpected injection error: %v", method, err)
				}
				jrq, err := req.JsonRpcRequest(context.Background())
				if err != nil {
					t.Fatalf("%s: could not read back request: %v", method, err)
				}
				if !reflect.DeepEqual(jrq.Params, tc.want) {
					t.Errorf("%s%s: injected params = %#v, want %#v", method, tc.params, jrq.Params, tc.want)
				}
			}
		})
	}
}

// TestGetConfirmedBlockAlias_ProcessedClampParity pins the THIRD method-name
// table the alias has to appear in: atLeastConfirmedMethods, which drives
// clampCommitmentForMethod.
//
// getBlock and getConfirmedBlock both reject commitment=processed upstream
// (agave: -32602 "Method does not support commitment below `confirmed`"), so an
// operator-configured default of processed must be narrowed to confirmed for
// BOTH names. With the alias missing from atLeastConfirmedMethods the clamp is
// skipped and the raw processed default is injected, which means the deprecated
// name is rejected outright by every upstream while getBlock beside it works.
//
// Kept separate from TestGetConfirmedBlockAlias_CommitmentInjectionParity
// because the contract is different in kind: there the injected value IS the
// configured default, here it deliberately is NOT.
func TestGetConfirmedBlockAlias_ProcessedClampParity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params string
		want   []interface{}
	}{
		// Only the slot argument: the options object is appended at index 1 and
		// the processed default is clamped to the nearest legal level on the way in.
		{
			name:   "bare slot appends clamped commitment",
			params: `[100]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"commitment": "confirmed"},
			},
		},
		// The clamp applies on the merge path too, not just the append path.
		{
			name:   "existing options object receives clamped commitment",
			params: `[100, {"encoding":"json"}]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"encoding": "json", "commitment": "confirmed"},
			},
		},
		// The clamp covers only the INJECTED network default. A caller who
		// explicitly asks for processed is never silently upgraded — the
		// upstream's -32602 is the honest answer, and rewriting it would hand
		// back data the caller did not ask for.
		{
			name:   "explicit processed from the caller is not clamped",
			params: `[100, {"commitment":"processed"}]`,
			want: []interface{}{
				float64(100),
				map[string]interface{}{"commitment": "processed"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, method := range getBlockAliases {
				net := &fakeNetwork{cfg: &common.NetworkConfig{
					Architecture: common.ArchitectureSvm,
					Svm:          &common.SvmNetworkConfig{Commitment: "processed"},
				}}
				req := newReq(method, tc.params)
				if _, _, err := networkPreForward_injectCommitment(context.Background(), net, req); err != nil {
					t.Fatalf("%s: unexpected injection error: %v", method, err)
				}
				jrq, err := req.JsonRpcRequest(context.Background())
				if err != nil {
					t.Fatalf("%s: could not read back request: %v", method, err)
				}
				if !reflect.DeepEqual(jrq.Params, tc.want) {
					t.Errorf("%s%s at network default processed: injected params = %#v, want %#v", method, tc.params, jrq.Params, tc.want)
				}
			}
		})
	}
}
