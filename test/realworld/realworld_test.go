package realworld

import (
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

// TestRealWorld runs the catalogue scenarios (review §3): network-scoped
// ones concurrently on shared pods, pod-scoped ones each in their own phase
// with fresh pods and redis.
func TestRealWorld(t *testing.T) {
	root, err := os.MkdirTemp("", "erpc-realworld-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			fmt.Fprintf(os.Stderr, "realworld: run dir kept: %s\n", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	runPhase(t, root, "shared", 9101, []spec{x1()})
}

// X1: quiet chain with 1–3 deep reorgs; the oracle holds for every block on
// both pods.
func x1() spec {
	return spec{name: "X1", bound: healthyDelivery, body: func(s *scenario) {
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
			s.blocks(40 + rand.IntN(35))
			s.reorg(1 + rand.IntN(3))
		}
		s.blocks(25)
	}}
}
