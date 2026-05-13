package evm

import (
	"strings"
	"testing"
)

// TestClassifyChangingStorageResult walks the full assertion truth table for
// the "changingStorage" strategy: both empty-variants the upstream might
// return for "0x0 at head" and the same-as-prev variants the upstream might
// return for "stale prev-block value".
//
// Inputs are JSON-encoded result bytes — the same shape jrr.GetResultBytes()
// produces — so quoted hex strings are the production-realistic form.
func TestClassifyChangingStorageResult(t *testing.T) {
	tests := []struct {
		name       string
		result     string
		prev       string
		wantReady  bool
		wantStored string
	}{
		// =====================================================================
		// Empty-variant rejection (assertion 1: non-empty)
		// =====================================================================
		{name: "Empty_QuotedZero", result: `"0x0"`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_QuotedBareHex", result: `"0x"`, prev: `"0xabc"`, wantReady: false},
		// Quoted uppercase-X variants of length >4 are classified as empty by
		// util.IsBytesEmptyish; the 4-byte "0X" exact form is an existing
		// asymmetric quirk of that helper and is not exercised here.
		{name: "Empty_QuotedUppercaseX_Zero", result: `"0X0"`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_NullLiteral", result: `null`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_QuotedEmptyString", result: `""`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_JsonEmptyArray", result: `[]`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_JsonEmptyObject", result: `{}`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_ZeroByte", result: `0`, prev: `"0xabc"`, wantReady: false},
		{name: "Empty_ZeroBytesInput", result: ``, prev: `"0xabc"`, wantReady: false},
		{
			name:      "Empty_32ByteZeroPadded",
			result:    `"0x0000000000000000000000000000000000000000000000000000000000000000"`,
			prev:      `"0xabc"`,
			wantReady: false,
		},
		{
			name:      "Empty_32ByteZeroPadded_UppercaseX",
			result:    `"0X0000000000000000000000000000000000000000000000000000000000000000"`,
			prev:      `"0xabc"`,
			wantReady: false,
		},

		// =====================================================================
		// Same-as-prev rejection (assertion 2: differs)
		// =====================================================================
		{
			name:      "SameAsPrev_Short",
			result:    `"0xde0b6b3a7640000"`,
			prev:      `"0xde0b6b3a7640000"`,
			wantReady: false,
		},
		{
			name:      "SameAsPrev_32ByteWord",
			result:    `"0x000000000000000000000000000000000000000000000a968163f0a57b400000"`,
			prev:      `"0x000000000000000000000000000000000000000000000a968163f0a57b400000"`,
			wantReady: false,
		},

		// =====================================================================
		// Healthy path: non-empty AND changed
		// =====================================================================
		{
			name:       "Changed_Short",
			result:     `"0xde0b6b3a7640001"`,
			prev:       `"0xde0b6b3a7640000"`,
			wantReady:  true,
			wantStored: `"0xde0b6b3a7640001"`,
		},
		{
			name:       "Changed_32ByteWord",
			result:     `"0x000000000000000000000000000000000000000000000a968163f0a57b400001"`,
			prev:       `"0x000000000000000000000000000000000000000000000a968163f0a57b400000"`,
			wantReady:  true,
			wantStored: `"0x000000000000000000000000000000000000000000000a968163f0a57b400001"`,
		},
		{
			// Crucially: a zero-padded SMALL non-zero value should be ready —
			// IsBytesEmptyish must trim leading zeros and see the remainder.
			name:       "Changed_ZeroPaddedTinyValue",
			result:     `"0x0000000000000000000000000000000000000000000000000000000000000001"`,
			prev:       `"0x0000000000000000000000000000000000000000000000000000000000000002"`,
			wantReady:  true,
			wantStored: `"0x0000000000000000000000000000000000000000000000000000000000000001"`,
		},
		{
			name:       "Changed_CaseSensitiveStringDiffers",
			result:     `"0xABC"`,
			prev:       `"0xabc"`,
			wantReady:  true, // byte-equal check; differs because A != a
			wantStored: `"0xABC"`,
		},

		// =====================================================================
		// Cold start (prev == "")
		// =====================================================================
		{
			name:       "ColdStart_AcceptsNonEmpty",
			result:     `"0xde0b6b3a7640000"`,
			prev:       ``,
			wantReady:  true,
			wantStored: `"0xde0b6b3a7640000"`,
		},
		{
			// Even on cold start, an empty result is rejected by assertion 1.
			name:      "ColdStart_RejectsEmpty",
			result:    `"0x0"`,
			prev:      ``,
			wantReady: false,
		},
		{
			name:       "ColdStart_AcceptsLongHex",
			result:     `"0x000000000000000000000000000000000000000000000a968163f0a57b400000"`,
			prev:       ``,
			wantReady:  true,
			wantStored: `"0x000000000000000000000000000000000000000000000a968163f0a57b400000"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored, ready := classifyChangingStorageResult([]byte(tt.result), tt.prev)
			if ready != tt.wantReady {
				t.Fatalf("ready = %v, want %v (result=%s prev=%s)", ready, tt.wantReady, tt.result, tt.prev)
			}
			if ready && stored != tt.wantStored {
				t.Fatalf("stored = %q, want %q", stored, tt.wantStored)
			}
			if !ready && stored != "" {
				t.Fatalf("stored should be empty when not ready, got %q", stored)
			}
		})
	}
}

// TestClassifyChangingStorageResult_NilBytes documents nil-bytes handling
// explicitly so callers can rely on it (len(nil) == 0, treated as empty).
func TestClassifyChangingStorageResult_NilBytes(t *testing.T) {
	stored, ready := classifyChangingStorageResult(nil, "")
	if ready {
		t.Fatalf("nil bytes must not be ready")
	}
	if stored != "" {
		t.Fatalf("nil bytes must produce empty stored, got %q", stored)
	}
}

// TestClassifyChangingStorageResult_ReturnToPriorValue confirms that the
// strategy doesn't penalize legitimate value oscillation. If a slot reads
// A → B → A across cycles (e.g. a per-block pendingNonce that rotates),
// each transition is "ready" because each result differs from the immediately
// previous one.
func TestClassifyChangingStorageResult_ReturnToPriorValue(t *testing.T) {
	prev := ""

	// Cycle 1: cold start with value A → ready, prev = A.
	stored, ready := classifyChangingStorageResult([]byte(`"0xa"`), prev)
	if !ready {
		t.Fatalf("cycle 1 must be ready")
	}
	prev = stored

	// Cycle 2: value B → ready, prev = B.
	stored, ready = classifyChangingStorageResult([]byte(`"0xb"`), prev)
	if !ready {
		t.Fatalf("cycle 2 must be ready (A → B)")
	}
	prev = stored

	// Cycle 3: back to value A → ready (differs from prev=B).
	stored, ready = classifyChangingStorageResult([]byte(`"0xa"`), prev)
	if !ready {
		t.Fatalf("cycle 3 must be ready (B → A is a change vs prev=B)")
	}
	prev = stored

	// Cycle 4: A again → NOT ready (same as prev=A).
	_, ready = classifyChangingStorageResult([]byte(`"0xa"`), prev)
	if ready {
		t.Fatalf("cycle 4 must NOT be ready (A → A same as prev)")
	}
}

// TestClassifyChangingStorageResult_StuckUpstream confirms that an upstream
// returning the same non-empty value across many cycles is rejected after
// the first one (only the initial cycle advances, subsequent ones don't).
func TestClassifyChangingStorageResult_StuckUpstream(t *testing.T) {
	const stuck = `"0xde0b6b3a7640000"`
	prev := ""

	// Cycle 1: cold start, value accepted.
	stored, ready := classifyChangingStorageResult([]byte(stuck), prev)
	if !ready {
		t.Fatalf("cycle 1 (cold start) must be ready")
	}
	prev = stored

	// Cycles 2-10: same value repeatedly → all rejected, prev unchanged.
	for i := 2; i <= 10; i++ {
		_, ready = classifyChangingStorageResult([]byte(stuck), prev)
		if ready {
			t.Fatalf("cycle %d returning same value must NOT be ready", i)
		}
	}
}

// TestClassifyChangingStorageResult_RecoveryAfterStall confirms that an
// upstream can recover: after a stretch of same-as-prev responses, the first
// response that finally differs is accepted.
func TestClassifyChangingStorageResult_RecoveryAfterStall(t *testing.T) {
	prev := ""

	// Cold start, accept value A.
	stored, ready := classifyChangingStorageResult([]byte(`"0xa"`), prev)
	if !ready {
		t.Fatalf("cold start must be ready")
	}
	prev = stored

	// Five cycles of A → all rejected.
	for i := 0; i < 5; i++ {
		_, ready = classifyChangingStorageResult([]byte(`"0xa"`), prev)
		if ready {
			t.Fatalf("stuck cycle %d must not be ready", i)
		}
	}
	// prev unchanged across all of those — caller's invariant.
	if prev != `"0xa"` {
		t.Fatalf("prev must remain unchanged across rejected cycles, got %q", prev)
	}

	// Sixth cycle: upstream recovers, returns B → accepted, prev advances.
	stored, ready = classifyChangingStorageResult([]byte(`"0xb"`), prev)
	if !ready {
		t.Fatalf("recovery cycle must be ready")
	}
	if stored != `"0xb"` {
		t.Fatalf("recovery cycle stored = %q, want %q", stored, `"0xb"`)
	}
}

// TestClassifyChangingStorageResult_TenderlyTraceScenario reconstructs the
// exact 3-cycle value sequence from the captured trace. Cycle 3 simulates the
// misbehaving upstream returning either 0x0 (literal trie miss) or the
// prev-block value (silent snapshot fallback) — both must be rejected.
func TestClassifyChangingStorageResult_TenderlyTraceScenario(t *testing.T) {
	// Cycle 1 — cold start, totalSupply has some value at block N.
	stored, ready := classifyChangingStorageResult([]byte(`"0xde0b6b3a7640000"`), "")
	if !ready {
		t.Fatalf("cold-start non-empty must be ready")
	}
	prev := stored

	// Cycle 2 — healthy upstream advances; totalSupply changes at block N+1.
	stored, ready = classifyChangingStorageResult([]byte(`"0xde0b6b3a7640001"`), prev)
	if !ready {
		t.Fatalf("healthy upstream with changed value must be ready")
	}
	prev = stored

	// Cycle 3a — misbehaving upstream returns 0x0 (trie node not committed).
	_, readyA := classifyChangingStorageResult([]byte(`"0x0"`), prev)
	if readyA {
		t.Fatalf("0x0 at advertised head must NOT advance stateReadyBlock")
	}

	// Cycle 3b — misbehaving upstream returns the prev-block value silently.
	_, readyB := classifyChangingStorageResult([]byte(`"0xde0b6b3a7640001"`), prev)
	if readyB {
		t.Fatalf("same-as-prev result must NOT advance stateReadyBlock")
	}

	// Cycle 3c — misbehaving upstream returns 32-byte zero-padded zero.
	_, readyC := classifyChangingStorageResult(
		[]byte(`"0x0000000000000000000000000000000000000000000000000000000000000000"`),
		prev,
	)
	if readyC {
		t.Fatalf("32-byte zero-padded result must NOT advance stateReadyBlock")
	}
}

// TestClassifyChangingStorageResult_PrevPersistsAcrossRejections confirms that
// the caller's stored-prev contract works: when an assertion fails, the
// returned stored value is empty so the caller leaves prev unchanged.
func TestClassifyChangingStorageResult_PrevPersistsAcrossRejections(t *testing.T) {
	prev := `"0xa"`

	for _, badResult := range []string{`"0x0"`, `null`, `"0xa"`, ``} {
		stored, ready := classifyChangingStorageResult([]byte(badResult), prev)
		if ready {
			t.Fatalf("bad result %q should not be ready", badResult)
		}
		if stored != "" {
			t.Fatalf("bad result %q should produce empty stored, got %q", badResult, stored)
		}
		// Caller would leave prev as-is — assertion is on this function's contract.
	}
}

// TestClassifyChangingStorageResult_LongValuesDoNotAlias confirms that two
// long hex values that share a long zero prefix but differ in the last byte
// are correctly classified as "differs". (Defensive against accidental
// prefix-comparison bugs.)
func TestClassifyChangingStorageResult_LongValuesDoNotAlias(t *testing.T) {
	a := `"0x000000000000000000000000000000000000000000000000000000000000000a"`
	b := `"0x000000000000000000000000000000000000000000000000000000000000000b"`

	stored, ready := classifyChangingStorageResult([]byte(b), a)
	if !ready {
		t.Fatalf("differing in last byte must be ready")
	}
	if stored != b {
		t.Fatalf("stored mismatch")
	}
}

// TestClassifyChangingStorageResult_QuoteSensitivity confirms the function
// treats quoted and unquoted forms as distinct (production-realistic inputs
// are quoted; this documents that the function isn't normalizing).
func TestClassifyChangingStorageResult_QuoteSensitivity(t *testing.T) {
	// Quoted "0xa" vs unquoted 0xa — strict string equality.
	stored, ready := classifyChangingStorageResult([]byte(`"0xa"`), `0xa`)
	if !ready {
		t.Fatalf(`"0xa" should differ from unquoted 0xa under strict equality`)
	}
	if !strings.Contains(stored, "0xa") {
		t.Fatalf("stored should contain 0xa")
	}
}
