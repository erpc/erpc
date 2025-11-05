package websocket

import (
	"testing"
)

func TestGenerateConnectionId(t *testing.T) {
	id1 := generateConnectionId()
	id2 := generateConnectionId()

	if id1 == id2 {
		t.Error("generateConnectionId should generate unique IDs")
	}

	if len(id1) != 32 {
		t.Errorf("expected ID length 32, got %d", len(id1))
	}
}

func TestConnection_ID(t *testing.T) {
	// This would require a mock websocket connection
	// Full implementation in integration tests
	t.Skip("Requires integration test setup")
}
