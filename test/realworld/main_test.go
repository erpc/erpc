// Package realworld runs the chain tracker end to end: real eRPC binaries
// (built with -race when REALWORLD_RACE=1), a real redis-server, real
// sockets through fault proxies, and real clients. Only the vendor nodes
// are simulated (test/chainsim).
package realworld

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// tapProcessingSlack: a tap waits at most W after the block's first
	// header, plus the tracker's processing (§15.1): a silent voter leaves
	// the hash voters at W exactly, and the send follows.
	tapProcessingSlack = 50 * time.Millisecond
	// healthyDelivery bounds a block's first header on a pod → client
	// receipt, and any wait between two receipts (spec §15.1, U3, X9).
	healthyDelivery = liveBound + tapProcessingSlack
	// otherInstanceWait: another instance answers a delivered block by
	// number within W, plus the tracker's processing (§19 oracle): both
	// follow the same voters, so they disagree on a branch for at most W.
	otherInstanceWait = liveBound + tapProcessingSlack
	// settleDelivery adds one settle round, itself within a live bound, for
	// sources that disagree (§6 rule 5, §13.1 "sources disagree: 1 settle").
	settleDelivery = healthyDelivery + liveBound
	// deepReorgDelivery adds reorgWindow blocks of walk (§13.1 "reorg
	// deeper than the window").
	deepReorgDelivery = healthyDelivery + reorgWindow*blockTime
	// grpcReadWait: a StreamBlocks Read stalled this long ends UNAVAILABLE
	// (§9.2).
	grpcReadWait = 60 * time.Second
	// grpcSendWait: one StreamBlocks Send blocked this long ends UNAVAILABLE
	// (§9.2).
	grpcSendWait = 10 * time.Second
	// grpcConnAge: the server's connection age, 10 min less grpc-go's 10%
	// jitter, ends every stream with UNAVAILABLE; a client resumes from
	// its cursor and misses nothing (§10.2, #977). An UNAVAILABLE end of a
	// stream younger than this is a stall or send wait.
	grpcConnAge = 9 * time.Minute
	// logsCloseSpread: 1013 closes from one event spread over 60s (§9.2).
	logsCloseSpread = 60 * time.Second
	// silentReconnect: a half-open wss source reconnects within 30s of its
	// head counter passing its last push (§13.1).
	silentReconnect = 30 * time.Second
	// minReconnectBackoff: wss reconnects back off 1–30s, jittered (§13.1).
	minReconnectBackoff = time.Second
	// ageGaugeEvery: eRPC sets every report-age gauge once a second
	// (health/tracker.go reportAgesEvery), so a scrape reads an age up to
	// this much old; the spec bounds the age, not the gauge (§17).
	ageGaugeEvery = time.Second

	// Harness slack, not spec: one metrics scrape interval, and the time a
	// client needs to notice an end and dial again.
	scrapeEvery   = 500 * time.Millisecond
	clientRedial  = time.Second
	httpCheckWait = 10 * time.Second
	podBootWait   = 90 * time.Second
	finalSettle   = 30 * time.Second
)

// erpcBin is the eRPC binary built from this worktree, with
// testdata/realworld_vars.go added to cmd/erpc; oldBin, built only when a
// selected phase runs old pods (X7), is oldImageRef's.
var erpcBin, oldBin string

// raceBuild: REALWORLD_RACE=1 builds erpcBin with -race; a pod's data race
// report then fails its phase (harness.stop).
var raceBuild = os.Getenv("REALWORLD_RACE") == "1"

// oldImageRef is the image the branch replaces (§18 rollout).
const oldImageRef = "origin/main"

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
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {
		filepath.Join(root, "cmd", "erpc", "realworld_vars.go"): filepath.Join(root, "test", "realworld", "testdata", "realworld_vars.go"),
	}})
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "overlay.json"), overlay, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Race coverage lives in the Go packages' -race tests; a -race pod costs
	// several times the CPU and memory, which caps how many phases run at once.
	args := []string{"build", "-overlay", filepath.Join(dir, "overlay.json"), "-o", erpcBin, "./cmd/erpc"}
	if raceBuild {
		args = slices.Insert(args, 1, "-race")
	}
	build := exec.Command("go", args...)
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	start := time.Now()
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "realworld: building the eRPC binary failed:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "realworld: built %s (race %t) in %s\n", erpcBin, raceBuild, time.Since(start).Round(time.Second))
	if needsOldImage() {
		oldBin = filepath.Join(dir, "erpc-old")
		start := time.Now()
		if err := buildOldImage(root, oldBin); err != nil {
			fmt.Fprintln(os.Stderr, "realworld: building the old image failed:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "realworld: built %s from %s in %s\n", oldBin, oldImageRef, time.Since(start).Round(time.Second))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// needsOldImage reports whether a phase this run selects runs old pods; a
// selection error is TestRealWorld's to report.
func needsOldImage() bool {
	phases, _, _ := selectedPhases()
	for _, ph := range phases {
		if len(ph.specs[0].oldPods) > 0 {
			return true
		}
	}
	return false
}

// buildOldImage builds oldImageRef's cmd/erpc into bin from a detached
// temporary worktree, removed after. Without -race: a race in the old image
// is not the branch's to find.
func buildOldImage(root, bin string) error {
	wt, err := os.MkdirTemp("", "erpc-realworld-old-")
	if err != nil {
		return err
	}
	run := func(dir string, args ...string) error {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	if err := run(root, "git", "worktree", "add", "--detach", wt, oldImageRef); err != nil {
		_ = os.RemoveAll(wt)
		return err
	}
	defer func() {
		_ = run(root, "git", "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
	}()
	return run(wt, "go", "build", "-o", bin, "./cmd/erpc")
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
