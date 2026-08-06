package integrity

import "testing"

func TestCheck_SchemaConformance(t *testing.T) {
	cs := only("schemaConformance", nil)
	// well-formed receipts array → ok
	if err := run(t, MethodGetBlockReceipts, []byte(`[{"transactionHash":"0x1","logs":[]}]`), cs); err != nil {
		t.Fatalf("well-formed receipts should pass: %v", err)
	}
	// a JSON object where an array is expected → rejected
	assertRejected(t, run(t, MethodGetBlockReceipts, []byte(`{"unexpected":"object"}`), cs))
	// well-formed block object → ok
	if err := run(t, MethodGetBlockByNumber, []byte(`{"hash":"0xaa","number":"0x1"}`), cs); err != nil {
		t.Fatalf("well-formed block should pass: %v", err)
	}
	// a JSON array where a block object is expected → rejected
	assertRejected(t, run(t, MethodGetBlockByNumber, []byte(`["not","a","block"]`), cs))
}
