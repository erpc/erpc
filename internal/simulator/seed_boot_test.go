package simulator

import (
	"strings"
	"testing"
)

// TestSeedYAMLExpanded_IsValid verifies the init() expansion produces a
// fully-formed YAML that decodes cleanly through eRPC's config
// validator — i.e. the simulator can boot from it on first start
// without any further substitution.
func TestSeedYAMLExpanded_IsValid(t *testing.T) {
	if strings.Contains(SeedYAMLExpanded, "{SELECTION_POLICY_FUNC}") {
		t.Fatalf("placeholder still present in SeedYAMLExpanded — init() didn't expand")
	}
	if !strings.Contains(SeedYAMLExpanded, "evalFunc: |") {
		t.Fatalf("expected literal-block evalFunc style, got:\n%s", SeedYAMLExpanded)
	}
	if !strings.Contains(SeedYAMLExpanded, ".removeCordoned()") {
		t.Fatalf("default policy body not present in expanded form")
	}
	cfg, err := DecodeConfigYAML([]byte(SeedYAMLExpanded))
	if err != nil {
		t.Fatalf("decode SeedYAMLExpanded: %v", err)
	}
	found := false
	for _, p := range cfg.Projects {
		for _, n := range p.Networks {
			if n.SelectionPolicy != nil && strings.Contains(n.SelectionPolicy.EvalFunc, ".removeCordoned()") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("selectionPolicy.evalFunc not parsed out of expanded SeedYAML")
	}
	// SeedYAML (template) MUST retain the placeholder — only the
	// EXPANDED variant gets the substitution. The template is what
	// the WS snapshot returns as `defaultYaml`, which the frontend
	// uses for its "↺ default" button (templated, editor-clean view).
	if !strings.Contains(SeedYAML, "{SELECTION_POLICY_FUNC}") {
		t.Fatalf("template SeedYAML should keep the placeholder; it's what defaultYaml exposes")
	}
	// Comments survive the expansion (used to teach eRPC concepts).
	if !strings.Contains(SeedYAMLExpanded, "# IMPORTANT: failsafe rules match TOP-DOWN") {
		t.Fatalf("teaching comment lost during init() expansion")
	}
}
