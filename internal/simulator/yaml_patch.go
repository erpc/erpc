package simulator

import (
	"bytes"
	"fmt"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// PolicyPlaceholder is the token the FRONTEND uses in its YAML editor in
// place of the selection-policy function body. The frontend substitutes
// it with the policy editor's source before sending an apply-config
// frame to the backend. The backend has NO runtime knowledge of this
// token — it only appears here so the same constant can be embedded
// into the SeedYAML at compile time (via a one-time init expansion)
// and inserted by the frontend on incoming-yaml templatization.
const PolicyPlaceholder = "{SELECTION_POLICY_FUNC}"

// expandPolicyPlaceholder substitutes occurrences of the placeholder
// with the supplied policy JS, emitted as a YAML literal-block scalar
// (`evalFunc: |`) so multi-line JS survives indentation cleanly.
//
// Used at INIT TIME only (to build the simulator's first-boot
// SeedYAMLExpanded from the templated SeedYAML). The runtime apply /
// validate paths do NOT call this — they treat incoming YAML as
// already-expanded text. The frontend is responsible for substitution
// before sending.
func expandPolicyPlaceholder(yamlSrc, policyJS string) string {
	if !strings.Contains(yamlSrc, PolicyPlaceholder) {
		return yamlSrc
	}
	lines := strings.Split(yamlSrc, "\n")
	for i, ln := range lines {
		idx := strings.Index(ln, "evalFunc:")
		if idx < 0 || !strings.Contains(ln, PolicyPlaceholder) {
			continue
		}
		indent := ln[:idx]
		bodyIndent := indent + "  "
		body := strings.Split(strings.TrimRight(policyJS, "\n"), "\n")
		out := []string{indent + "evalFunc: |"}
		for _, bl := range body {
			out = append(out, bodyIndent+bl)
		}
		lines[i] = strings.Join(out, "\n")
	}
	expanded := strings.Join(lines, "\n")
	// Defensive: leftover placeholder (no surrounding evalFunc:) gets
	// neutralized so downstream YAML parse doesn't fail with
	// "unexpected character".
	if strings.Contains(expanded, PolicyPlaceholder) {
		expanded = strings.ReplaceAll(expanded, PolicyPlaceholder, "/* policy placeholder unresolved */")
	}
	return expanded
}

// injectPolicyIntoYAML rewrites the first `selectionPolicy.evalFunc`
// field on the first network of the first project to the supplied JS
// source, emitted as a literal-block scalar. Used by ApplyPolicy so the
// stored YAML stays self-contained — every apply produces a YAML that,
// in isolation, fully expresses the running config without needing any
// placeholder substitution machinery.
//
// Note: this round-trips through yaml.v3's Node API, which preserves
// most comments (head/foot/line on nodes), but may not be byte-perfect.
// That's acceptable for ApplyPolicy: it's a deliberate user action
// where the result is also visible in the editor.
func injectPolicyIntoYAML(yamlSrc, policyJS string) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(yamlSrc), &root); err != nil {
		return "", fmt.Errorf("yaml unmarshal: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return "", fmt.Errorf("yaml: empty document")
	}
	top := root.Content[0]
	projects := mapValue(top, "projects")
	if projects == nil || projects.Kind != yaml.SequenceNode || len(projects.Content) == 0 {
		return "", fmt.Errorf("yaml: missing projects[]")
	}
	networks := mapValue(projects.Content[0], "networks")
	if networks == nil || networks.Kind != yaml.SequenceNode || len(networks.Content) == 0 {
		return "", fmt.Errorf("yaml: missing networks[]")
	}
	net := networks.Content[0]
	sp := mapValue(net, "selectionPolicy")
	if sp == nil {
		net.Content = append(net.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "selectionPolicy"},
			&yaml.Node{Kind: yaml.MappingNode},
		)
		sp = net.Content[len(net.Content)-1]
	}
	ef := mapValue(sp, "evalFunc")
	if ef == nil {
		sp.Content = append(sp.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "evalFunc"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: policyJS},
		)
	} else {
		ef.Tag = "!!str"
		ef.Style = yaml.LiteralStyle
		ef.Value = policyJS
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return "", fmt.Errorf("yaml marshal: %w", err)
	}
	_ = enc.Close()
	return buf.String(), nil
}

// mapValue returns the value-node for the given key on a MappingNode, or
// nil if the key isn't present.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
