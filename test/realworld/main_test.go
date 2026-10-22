// Package realworld runs the chain tracker end to end: real eRPC binaries
// built with -race, a real redis-server, real sockets through fault
// proxies, and real clients. Only the vendor nodes are simulated
// (test/chainsim).
package realworld

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Timing of every simulated chain and the tolerances the oracle applies,
// each from the spec section that states it (specs/chain-tracker/feature.md).
const (
	blockTime = 200 * time.Millisecond
	// reorgWindow and maxBackfill are each scenario network's
	// chainTracker config (§14); maxBackfill is its 5m default, so a gRPC
	// cursor from a pod cut off for 2 minutes (N4) is still inside it.
	reorgWindow = 16
	maxBackfill = 5 * time.Minute

	// liveBound: max(3 × block time, 2s) (§1). A live source's head is
	// pinned, and sent, within it.
	liveBound = 2 * time.Second
	// healthyDelivery bounds mint → client receipt, and any wait between
	// two receipts, while a live source serves the chain (§1, §5).
	healthyDelivery = liveBound
	// settleDelivery adds one settle round, itself within a live bound, for
	// sources that disagree (§6 rule 5, §13.1 "sources disagree: 1 settle").
	settleDelivery = 2 * liveBound
	// deepReorgDelivery adds reorgWindow blocks of walk (§13.1 "reorg
	// deeper than the window").
	deepReorgDelivery = liveBound + reorgWindow*blockTime
	// grpcReadWait: a StreamBlocks Read stalled this long ends UNAVAILABLE
	// (§9.2).
	grpcReadWait = 60 * time.Second
	// grpcSendWait: one StreamBlocks Send blocked this long ends UNAVAILABLE
	// (§9.2).
	grpcSendWait = 10 * time.Second
	// logsCloseSpread: 1013 closes from one event spread over 60s (§9.2).
	logsCloseSpread = 60 * time.Second
	// silentReconnect: a half-open wss source reconnects within 30s of its
	// head counter passing its last push (§13.1).
	silentReconnect = 30 * time.Second
	// minReconnectBackoff: wss reconnects back off 1–30s, jittered (§13.1).
	minReconnectBackoff = time.Second

	// Harness slack, not spec: one metrics scrape interval, and the time a
	// client needs to notice an end and dial again.
	scrapeEvery   = 500 * time.Millisecond
	clientRedial  = time.Second
	httpCheckWait = 10 * time.Second
	podBootWait   = 90 * time.Second
	finalSettle   = 30 * time.Second
)

// erpcBin is the -race eRPC binary built from this worktree.
var erpcBin string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("redis-server"); err != nil {
		fmt.Fprintln(os.Stderr, "realworld: redis-server not found in PATH; install it (macOS: brew install redis; Debian: apt install redis-server)")
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "erpc-realworld-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	erpcBin = filepath.Join(dir, "erpc")
	build := exec.Command("go", "build", "-race", "-o", erpcBin, "./cmd/erpc")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	start := time.Now()
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "realworld: building the -race eRPC binary failed:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "realworld: built %s in %s\n", erpcBin, time.Since(start).Round(time.Second))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", fmt.Errorf("realworld: not inside the erpc module")
	}
	return filepath.Dir(mod), nil
}
