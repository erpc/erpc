package indexer

import (
	"strings"
	"testing"
)

func TestBuildParamsKey_StableAcrossCalls(t *testing.T) {
	a := BuildParamsKey([]interface{}{"logs", map[string]interface{}{"topics": []string{"0x1"}}})
	b := BuildParamsKey([]interface{}{"logs", map[string]interface{}{"topics": []string{"0x1"}}})
	if a != b {
		t.Fatalf("same params must hash identically, got %q vs %q", a, b)
	}
}

func TestBuildParamsKey_DifferentParamsDiffer(t *testing.T) {
	a := BuildParamsKey([]interface{}{"logs", map[string]interface{}{"topics": []string{"0x1"}}})
	b := BuildParamsKey([]interface{}{"logs", map[string]interface{}{"topics": []string{"0x2"}}})
	if a == b {
		t.Fatalf("different params must produce different keys")
	}
}

func TestExtractSubscriptionType(t *testing.T) {
	cases := []struct {
		name   string
		params []interface{}
		want   string
	}{
		{"newHeads", []interface{}{"newHeads"}, "newHeads"},
		{"logs with filter", []interface{}{"logs", map[string]interface{}{}}, "logs"},
		{"empty params", []interface{}{}, ""},
		{"non-string first", []interface{}{123}, ""},
	}
	for _, c := range cases {
		if got := ExtractSubscriptionType(c.params); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestExtractClientSubID_ValidString(t *testing.T) {
	got, err := ExtractClientSubID([]interface{}{"0xabc"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "0xabc" {
		t.Fatalf("got %q want 0xabc", got)
	}
}

func TestExtractClientSubID_EmptyParams(t *testing.T) {
	_, err := ExtractClientSubID(nil)
	if err == nil {
		t.Fatalf("expected error on empty params")
	}
}

func TestExtractClientSubID_NonString(t *testing.T) {
	_, err := ExtractClientSubID([]interface{}{123})
	if err == nil {
		t.Fatalf("expected error on non-string param")
	}
}

func TestGenerateClientSubID_Shape(t *testing.T) {
	id, err := GenerateClientSubID()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.HasPrefix(id, "0x") {
		t.Fatalf("id must start with 0x: %q", id)
	}
	// 2 for "0x" + 32 hex chars (16 bytes * 2)
	if len(id) != 34 {
		t.Fatalf("id must be 34 chars, got %d (%q)", len(id), id)
	}
}

func TestGenerateClientSubID_Unique(t *testing.T) {
	a, _ := GenerateClientSubID()
	b, _ := GenerateClientSubID()
	if a == b {
		t.Fatalf("two consecutive IDs must differ")
	}
}
