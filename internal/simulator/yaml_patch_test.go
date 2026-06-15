package simulator

import (
	"strings"
	"testing"
)

// TestExpandPolicyPlaceholder verifies the init-time helper used to
// build SeedYAMLExpanded — substitution emits a literal-block scalar
// and preserves surrounding comments + indentation.
func TestExpandPolicyPlaceholder(t *testing.T) {
	yamlSrc := `projects:
  - id: sim
    networks:
      - architecture: evm
        # comment preserved
        selectionPolicy:
          evalInterval: 1s
          evalFunc: "{SELECTION_POLICY_FUNC}"
`
	policyJS := "(u, c) =>\n  u.removeCordoned()\n"
	got := expandPolicyPlaceholder(yamlSrc, policyJS)
	if strings.Contains(got, "{SELECTION_POLICY_FUNC}") {
		t.Fatalf("placeholder not substituted:\n%s", got)
	}
	if !strings.Contains(got, "evalFunc: |") {
		t.Fatalf("expected literal-block style, got:\n%s", got)
	}
	if !strings.Contains(got, "(u, c) =>") {
		t.Fatalf("policy body missing:\n%s", got)
	}
	if !strings.Contains(got, "# comment preserved") {
		t.Fatalf("top-of-block comment lost:\n%s", got)
	}
}

// TestInjectPolicyIntoYAML verifies ApplyPolicy's underlying helper —
// it must overwrite an existing evalFunc and produce a parseable YAML.
func TestInjectPolicyIntoYAML(t *testing.T) {
	yamlSrc := `projects:
  - id: sim
    networks:
      - architecture: evm
        selectionPolicy:
          evalInterval: 1s
          evalFunc: |
            (u, c) => u
    upstreams:
      - id: foo
        endpoint: http://x
`
	out, err := injectPolicyIntoYAML(yamlSrc, "(u, c) => u.sortByScore(PREFER_FASTEST)")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "sortByScore(PREFER_FASTEST)") {
		t.Fatalf("new policy missing:\n%s", out)
	}
	if strings.Contains(out, "(u, c) => u\n") {
		t.Fatalf("old policy still present:\n%s", out)
	}
}
