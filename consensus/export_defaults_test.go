package consensus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// The integrity module hands its misbehaviorsDestination straight to the
// factory without running the consensus defaults chain. Before the factory
// applied defaults itself, an unset filePattern resolved every flush to the
// same ".jsonl" key — S3 uploads then overwrote each other silently.
func TestCreateMisbehaviorExporterAppliesDefaultsWithoutSetDefaults(t *testing.T) {
	dir := t.TempDir()
	log := zerolog.New(os.Stderr)
	cfg := &common.MisbehaviorsDestinationConfig{
		Type: common.MisbehaviorsDestinationTypeFile,
		Path: dir,
		// FilePattern deliberately unset — the integrity config path never
		// calls SetDefaults.
	}
	exp := createMisbehaviorExporter(cfg, &log)
	if exp == nil {
		t.Fatal("exporter failed to initialize")
	}
	if err := exp.AppendWithMetadata([]byte(`{"x":1}`), "eth_getBlockByHash", "evm:1"); err != nil {
		t.Fatalf("append failed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no export file written")
	}
	for _, e := range entries {
		name := filepath.Base(e.Name())
		if name == ".jsonl" || name == ".ndjson" {
			t.Fatalf("export file has an empty name stem (%q) — defaults were not applied", name)
		}
		if !strings.Contains(name, "eth_getBlockByHash") {
			t.Fatalf("expected the default pattern (timestampMs-method-networkId) in %q", name)
		}
	}
	// The caller's config must not be mutated (defaults go on a copy).
	if cfg.FilePattern != "" {
		t.Fatalf("caller config was mutated: FilePattern=%q", cfg.FilePattern)
	}
}
