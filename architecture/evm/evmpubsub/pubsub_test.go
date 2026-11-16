package evmpubsub

import (
	"strings"
	"testing"
)

func TestSubscribeKind(t *testing.T) {
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
		if got, _ := (Dialect{}).SubscribeKind(MethodSubscribe, c.params); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestUnsubscribeID_ValidString(t *testing.T) {
	got, err := Dialect{}.UnsubscribeID([]interface{}{"0xabc"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "0xabc" {
		t.Fatalf("got %q want 0xabc", got)
	}
}

func TestUnsubscribeID_EmptyParams(t *testing.T) {
	_, err := Dialect{}.UnsubscribeID(nil)
	if err == nil {
		t.Fatalf("expected error on empty params")
	}
}

func TestUnsubscribeID_NonString(t *testing.T) {
	_, err := Dialect{}.UnsubscribeID([]interface{}{123})
	if err == nil {
		t.Fatalf("expected error on non-string param")
	}
}

func TestNewSubID_Shape(t *testing.T) {
	id, err := Dialect{}.NewSubID()
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

func TestNewSubID_Unique(t *testing.T) {
	a, _ := Dialect{}.NewSubID()
	b, _ := Dialect{}.NewSubID()
	if a == b {
		t.Fatalf("two consecutive IDs must differ")
	}
}
