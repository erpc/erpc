package common

import (
	"strings"
	"testing"
)

func TestEvmStateProbeConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *EvmStateProbeConfig
		wantErr string // substring; "" means no error expected
	}{
		// =====================================================================
		// Nil + empty cases
		// =====================================================================
		{
			name:    "Nil_NoError",
			cfg:     nil,
			wantErr: "",
		},
		{
			name:    "AllFieldsZero_FailsOnStrategy",
			cfg:     &EvmStateProbeConfig{},
			wantErr: "stateProbe.strategy is required",
		},

		// =====================================================================
		// Strategy validation
		// =====================================================================
		{
			name: "UnknownStrategy_Errors",
			cfg: &EvmStateProbeConfig{
				Strategy: "nonEmptyCode", // valid future name, not in v1
				Address:  "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:     "0x3",
			},
			wantErr: `stateProbe.strategy "nonEmptyCode" is not a known strategy`,
		},
		{
			// Case sensitivity matters — strategy names are exact strings.
			name: "Strategy_CaseSensitive",
			cfg: &EvmStateProbeConfig{
				Strategy: "ChangingStorage", // capital C, wrong case
				Address:  "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:     "0x3",
			},
			wantErr: "is not a known strategy",
		},
		{
			name: "Strategy_WhitespacePadded_TreatedAsUnknown",
			cfg: &EvmStateProbeConfig{
				Strategy: " changingStorage ",
				Address:  "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:     "0x3",
			},
			wantErr: "is not a known strategy",
		},

		// =====================================================================
		// changingStorage — input requirements
		// =====================================================================
		{
			name: "ChangingStorage_MissingAddress",
			cfg: &EvmStateProbeConfig{
				Strategy: StateProbeChangingStorage,
				Slot:     "0x3",
			},
			wantErr: "stateProbe.address is required",
		},
		{
			name: "ChangingStorage_MissingSlot",
			cfg: &EvmStateProbeConfig{
				Strategy: StateProbeChangingStorage,
				Address:  "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
			},
			wantErr: "stateProbe.slot is required",
		},
		{
			// Order: strategy is checked first, then per-strategy inputs.
			// Missing both inputs — error should mention address (the first one).
			name: "ChangingStorage_BothMissing_ReportsAddressFirst",
			cfg: &EvmStateProbeConfig{
				Strategy: StateProbeChangingStorage,
			},
			wantErr: "stateProbe.address is required",
		},
		{
			name: "ChangingStorage_Complete_NoError",
			cfg: &EvmStateProbeConfig{
				Strategy: StateProbeChangingStorage,
				Address:  "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:     "0x3",
			},
			wantErr: "",
		},
		{
			name: "ChangingStorage_WithFailureThreshold",
			cfg: &EvmStateProbeConfig{
				Strategy:         StateProbeChangingStorage,
				Address:          "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:             "0x3",
				FailureThreshold: 25,
			},
			wantErr: "",
		},
		{
			// Negative FailureThreshold is accepted at validate-time (the poller
			// treats anything <=0 as "use default 10"). Documenting that contract.
			name: "ChangingStorage_NegativeFailureThreshold_Accepted",
			cfg: &EvmStateProbeConfig{
				Strategy:         StateProbeChangingStorage,
				Address:          "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
				Slot:             "0x3",
				FailureThreshold: -5,
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestEvmStateProbeConfig_Copy(t *testing.T) {
	t.Run("NilCopy_ReturnsNil", func(t *testing.T) {
		var c *EvmStateProbeConfig
		if got := c.Copy(); got != nil {
			t.Fatalf("nil copy should return nil, got %+v", got)
		}
	})

	t.Run("DeepCopy_Independent", func(t *testing.T) {
		orig := &EvmStateProbeConfig{
			Strategy:         StateProbeChangingStorage,
			Address:          "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
			Slot:             "0x3",
			FailureThreshold: 5,
		}
		dup := orig.Copy()

		if dup == orig {
			t.Fatalf("Copy must return a new pointer")
		}
		if dup.Strategy != orig.Strategy || dup.Address != orig.Address ||
			dup.Slot != orig.Slot || dup.FailureThreshold != orig.FailureThreshold {
			t.Fatalf("fields not copied faithfully: %+v vs %+v", dup, orig)
		}

		// Mutate dup; orig must be unaffected.
		dup.Address = "0xdead"
		if orig.Address == "0xdead" {
			t.Fatalf("mutation of copy leaked back to original")
		}
	})

	t.Run("Copy_PreservesZeroFailureThreshold", func(t *testing.T) {
		orig := &EvmStateProbeConfig{
			Strategy: StateProbeChangingStorage,
			Address:  "0xabc",
			Slot:     "0x1",
			// FailureThreshold left at zero (its zero value)
		}
		dup := orig.Copy()
		if dup.FailureThreshold != 0 {
			t.Fatalf("zero FailureThreshold should be preserved, got %d", dup.FailureThreshold)
		}
	})

	t.Run("Copy_PreservesEmptyStringFields", func(t *testing.T) {
		// Even with empty Address/Slot (config is invalid but Copy should be
		// byte-faithful) — Copy doesn't validate, it duplicates.
		orig := &EvmStateProbeConfig{Strategy: StateProbeChangingStorage}
		dup := orig.Copy()
		if dup.Address != "" || dup.Slot != "" {
			t.Fatalf("empty fields should be preserved as empty, got %+v", dup)
		}
	})

	t.Run("Copy_RoundTripStillValidates", func(t *testing.T) {
		orig := &EvmStateProbeConfig{
			Strategy:         StateProbeChangingStorage,
			Address:          "0xabc",
			Slot:             "0x1",
			FailureThreshold: 7,
		}
		if err := orig.Validate(); err != nil {
			t.Fatalf("orig should be valid: %v", err)
		}
		dup := orig.Copy()
		if err := dup.Validate(); err != nil {
			t.Fatalf("copy should also be valid: %v", err)
		}
	})

	t.Run("Copy_LongAddressPreserved", func(t *testing.T) {
		// Address is a string — Copy must preserve byte-exact value (no
		// case-normalization, no whitespace trimming).
		const oddAddr = "  0xMixedCaseAddressWithSpaces  "
		orig := &EvmStateProbeConfig{
			Strategy: StateProbeChangingStorage,
			Address:  oddAddr,
			Slot:     "0x3",
		}
		dup := orig.Copy()
		if dup.Address != oddAddr {
			t.Fatalf("Copy must preserve address byte-exact, got %q", dup.Address)
		}
	})
}

// TestEvmStateProbeStrategy_StringContract documents that the strategy
// constant's underlying string value is the wire format (used as-is in YAML
// configs). Changing this is a breaking config change.
func TestEvmStateProbeStrategy_StringContract(t *testing.T) {
	if got := string(StateProbeChangingStorage); got != "changingStorage" {
		t.Fatalf("StateProbeChangingStorage wire value drifted: got %q, want %q",
			got, "changingStorage")
	}
}
