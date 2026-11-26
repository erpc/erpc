package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// analyzerNode is a fake EVM node answering the probes the config analyzer
// sends: eth_chainId and eth_getBlockByNumber for "0x0", "latest",
// "finalized" and arbitrary hex heights. An empty field makes the matching
// probe fail with a JSON-RPC error.
type analyzerNode struct {
	chainId   string
	genesis   string
	latest    string
	finalized string
	// blockHash answers every other hex height.
	blockHash string

	mu    sync.Mutex
	calls map[string]int
}

func (n *analyzerNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     interface{}   `json:"id"`
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key := req.Method
	if len(req.Params) > 0 {
		key = fmt.Sprintf("%s(%v)", req.Method, req.Params[0])
	}
	n.mu.Lock()
	if n.calls == nil {
		n.calls = map[string]int{}
	}
	n.calls[key]++
	n.mu.Unlock()

	var result interface{}
	switch req.Method {
	case "eth_chainId":
		if n.chainId != "" {
			result = n.chainId
		}
	case "eth_getBlockByNumber":
		tag, _ := req.Params[0].(string)
		switch tag {
		case "0x0":
			if n.genesis != "" {
				result = map[string]interface{}{"number": "0x0", "hash": n.genesis}
			}
		case "latest":
			if n.latest != "" {
				result = map[string]interface{}{"number": n.latest, "hash": "0xlatest"}
			}
		case "finalized":
			if n.finalized != "" {
				result = map[string]interface{}{"number": n.finalized, "hash": "0xfinal"}
			}
		default:
			if n.blockHash != "" {
				result = map[string]interface{}{"number": tag, "hash": n.blockHash}
			}
		}
	}
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
	if result == nil {
		resp["error"] = map[string]interface{}{"code": -32601, "message": "the method is not available"}
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (n *analyzerNode) callCount(key string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[key]
}

func startAnalyzerNode(t *testing.T, n *analyzerNode) string {
	t.Helper()
	srv := httptest.NewServer(n)
	t.Cleanup(srv.Close)
	return srv.URL
}

func analyzerUpstream(id, endpoint string, chainId int64) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: endpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: chainId},
	}
}

func analyzerConfig(upstreams ...*common.UpstreamConfig) *common.Config {
	return &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{{
			Id: "main",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 1},
				Failsafe:     []*common.FailsafeConfig{{MatchMethod: "*"}},
			}},
			Upstreams: upstreams,
		}},
	}
}

// findingsWith returns the findings that contain every given fragment.
func findingsWith(findings []string, fragments ...string) []string {
	var out []string
	for _, f := range findings {
		all := true
		for _, frag := range fragments {
			if !strings.Contains(f, frag) {
				all = false
				break
			}
		}
		if all {
			out = append(out, f)
		}
	}
	return out
}

func resetGockForAnalyzer(t *testing.T) {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
}

func TestConfigAnalyzerReport_StaticFindings(t *testing.T) {
	enabled := true
	cfg := &common.Config{
		Metrics: &common.MetricsConfig{HistogramBuckets: "0.1,not-a-number"},
		RateLimiters: &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{
			{Id: "project-budget", Rules: []*common.RateLimitRuleConfig{{Method: "*"}, {Method: "eth_call"}}},
			{Id: "upstream-budget"},
			{Id: "tuned-budget"},
			{Id: "provider-budget"},
			{Id: "auth-budget"},
			{Id: "admin-budget"},
			{Id: "unused-budget"},
		}},
		Admin: &common.AdminConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, RateLimitBudget: "admin-budget"},
		}}},
		Projects: []*common.ProjectConfig{
			{
				Id:              "alpha",
				RateLimitBudget: "project-budget",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}, Alias: "mainnet"},
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 10}, Failsafe: []*common.FailsafeConfig{{MatchMethod: "*"}}},
				},
				Upstreams: []*common.UpstreamConfig{
					{Id: "on-10", Endpoint: " ", Evm: &common.EvmUpstreamConfig{ChainId: 10}, RateLimitBudget: "upstream-budget"},
					{Id: "unmatched-chain", Endpoint: "", Evm: &common.EvmUpstreamConfig{ChainId: 999}},
					{Id: "no-chain", Endpoint: "", RateLimitBudget: "tuned-budget", RateLimitAutoTune: &common.RateLimitAutoTuneConfig{Enabled: &enabled}},
				},
				Providers: []*common.ProviderConfig{{
					Id:        "prov",
					Overrides: map[string]*common.UpstreamConfig{"*": {RateLimitBudget: "provider-budget"}},
				}},
				Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
					{Type: common.AuthTypeSecret, RateLimitBudget: "auth-budget"},
				}},
			},
			{
				Id: "beta",
				Upstreams: []*common.UpstreamConfig{
					{Id: "lonely", Endpoint: ""},
				},
			},
		},
	}

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Equal(t, ValidationTotals{ProjectsTotal: 2, NetworksTotal: 2, UpstreamsTotal: 4, RateLimitBudgetsTotal: 7}, report.Resources.Totals)
	require.Len(t, report.Resources.Tree.Projects, 2)
	alpha := report.Resources.Tree.Projects[0]
	require.Len(t, alpha.Networks, 2)
	assert.Equal(t, NetworkNode{Id: "evm:1", Alias: "mainnet", Upstreams: []UpstreamNode{{Id: "unmatched-chain"}, {Id: "no-chain"}}, chainId: 1}, alpha.Networks[0],
		"upstreams with an unknown chain or no chain attach to the first network")
	assert.Equal(t, []UpstreamNode{{Id: "on-10"}}, alpha.Networks[1].Upstreams, "upstream attaches to the network with its chain id")
	assert.Empty(t, report.Resources.Tree.Projects[1].Networks, "a project without networks has no place to attach upstreams")
	assert.Equal(t, RateLimitBudgetNode{Id: "project-budget", RulesCount: 2}, report.Resources.Tree.RateLimiters.Budgets[0])

	assert.Equal(t, []string{"orphan rateLimit budget 'unused-budget'"}, findingsWith(report.Notices, "orphan"))
	assert.Len(t, findingsWith(report.Notices, "upstream=on-10", "auto-tune disabled"), 1)
	assert.Empty(t, findingsWith(report.Notices, "upstream=no-chain", "auto-tune disabled"), "auto-tune enabled is not reported")
	assert.Equal(t, []string{"project=alpha network=evm/1 has no failsafe policies (defaults and per-network empty)"}, findingsWith(report.Notices, "no failsafe"))

	assert.Len(t, findingsWith(report.Errors, "invalid metrics histogramBuckets"), 1)
	for _, id := range []string{"on-10", "unmatched-chain", "no-chain"} {
		assert.Len(t, findingsWith(report.Errors, "project 'alpha'", "upstream '"+id+"' has an empty endpoint URL"), 1, id)
	}
	assert.Len(t, findingsWith(report.Errors, "project 'beta'", "upstream 'lonely' has an empty endpoint URL"), 1)
	assert.Empty(t, report.Warnings)
}

func TestConfigAnalyzerReport_NetworkDefaultsFailsafeSuppressesNotice(t *testing.T) {
	cfg := &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{{
			Id:              "main",
			NetworkDefaults: &common.NetworkDefaults{Failsafe: []*common.FailsafeConfig{{MatchMethod: "*"}}},
			Networks:        []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}}},
		}},
	}
	report := GenerateValidationReport(context.Background(), cfg)
	assert.Empty(t, findingsWith(report.Notices, "no failsafe"))
	assert.Empty(t, report.Errors)
	assert.Empty(t, report.Warnings)
}

func TestConfigAnalyzerReport_ProxyPoolErrorSkipsProject(t *testing.T) {
	cfg := analyzerConfig(analyzerUpstream("a", "", 1))
	cfg.ProxyPools = []*common.ProxyPoolConfig{{ID: "empty"}}

	report := GenerateValidationReport(context.Background(), cfg)

	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0], "project=main failed to create proxy pool registry")
	assert.Contains(t, report.Errors[0], "no proxy URLs defined for pool 'empty'")
}

func TestConfigAnalyzerReport_HealthyUpstreamsProduceNoFindings(t *testing.T) {
	resetGockForAnalyzer(t)
	healthy := func() *analyzerNode {
		return &analyzerNode{chainId: "0x1", genesis: "0xGEN", latest: "0x2000", finalized: "0x1000", blockHash: "0xB"}
	}
	nodeA, nodeB := healthy(), healthy()
	nodeB.genesis = "0xgen" // hash comparison is case-insensitive
	cfg := analyzerConfig(
		analyzerUpstream("a", startAnalyzerNode(t, nodeA), 1),
		analyzerUpstream("b", startAnalyzerNode(t, nodeB), 1),
	)

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Empty(t, report.Errors)
	assert.Empty(t, report.Warnings)
	// finalized (0x1000) minus the 64-block safety margin
	assert.Equal(t, 1, nodeA.callCount("eth_getBlockByNumber(0xfc0)"))
	assert.Equal(t, 1, nodeB.callCount("eth_getBlockByNumber(0xfc0)"))
}

func TestConfigAnalyzerReport_StrongMajorityMismatchIsError(t *testing.T) {
	resetGockForAnalyzer(t)
	good := func() *analyzerNode {
		return &analyzerNode{chainId: "0x1", genesis: "0xgood", latest: "0x2000", finalized: "0x1000", blockHash: "0xblock"}
	}
	bad := good()
	bad.genesis = "0xfork"
	bad.blockHash = "0xforkblock"
	wrongChain := good()
	wrongChain.chainId = "0x5"
	cfg := analyzerConfig(
		analyzerUpstream("good1", startAnalyzerNode(t, good()), 1),
		analyzerUpstream("good2", startAnalyzerNode(t, good()), 1),
		analyzerUpstream("bad", startAnalyzerNode(t, bad), 1),
		analyzerUpstream("wrong-chain", startAnalyzerNode(t, wrongChain), 1),
	)

	report := GenerateValidationReport(context.Background(), cfg)

	assert.ElementsMatch(t, []string{
		"project=main upstream=bad chain=1 genesis hash mismatch (majority 2/3 agree on 0xgood) got=0xfork",
		"project=main upstream=bad chain=1 block=4032 hash mismatch (majority 2/3 agree on 0xblock) got=0xforkblock",
		"In project 'main', upstream 'wrong-chain' reported chain ID 5 but the configuration expects 1.",
	}, report.Errors)
	assert.Empty(t, report.Warnings)
}

func TestConfigAnalyzerReport_TwoWayMismatchIsWarning(t *testing.T) {
	resetGockForAnalyzer(t)
	// No finalized support: the comparison falls back to latest-1024, using
	// the lowest latest height across upstreams.
	a := &analyzerNode{chainId: "0x1", genesis: "0xaaa", latest: "0x900", blockHash: "0xha"}
	b := &analyzerNode{chainId: "0x1", genesis: "0xbbb", latest: "0x800", blockHash: "0xhb"}
	cfg := analyzerConfig(
		analyzerUpstream("a", startAnalyzerNode(t, a), 1),
		analyzerUpstream("b", startAnalyzerNode(t, b), 1),
	)

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Empty(t, report.Errors)
	assert.Len(t, findingsWith(report.Warnings, "genesis hash differs from other upstream(s)", "only 2 upstreams compared"), 1,
		"one of the two disagreeing upstreams is reported: %v", report.Warnings)
	assert.Len(t, findingsWith(report.Warnings, "block=1024 hash differs from other upstream(s)", "only 2 upstreams compared"), 1,
		"%v", report.Warnings)
	assert.Equal(t, 1, a.callCount("eth_getBlockByNumber(0x400)"))
}

func TestConfigAnalyzerReport_YoungChainSkipsBlockComparison(t *testing.T) {
	resetGockForAnalyzer(t)
	cases := []struct {
		name string
		node func() *analyzerNode
	}{
		{"latest below 1024", func() *analyzerNode {
			return &analyzerNode{chainId: "0x1", genesis: "0xg", latest: "0x10", blockHash: "0xh"}
		}},
		{"finalized below 64", func() *analyzerNode {
			return &analyzerNode{chainId: "0x1", genesis: "0xg", latest: "0x10", finalized: "0x20", blockHash: "0xh"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := tc.node(), tc.node()
			cfg := analyzerConfig(
				analyzerUpstream("a", startAnalyzerNode(t, a), 1),
				analyzerUpstream("b", startAnalyzerNode(t, b), 1),
			)
			report := GenerateValidationReport(context.Background(), cfg)
			assert.Empty(t, report.Errors)
			assert.Empty(t, report.Warnings)
			for _, n := range []*analyzerNode{a, b} {
				n.mu.Lock()
				for k := range n.calls {
					assert.Contains(t, []string{"eth_chainId", "eth_getBlockByNumber(0x0)", "eth_getBlockByNumber(latest)", "eth_getBlockByNumber(finalized)"}, k,
						"no historical block probe expected")
				}
				n.mu.Unlock()
			}
		})
	}
}

func TestConfigAnalyzerReport_FullNodeSkipsGenesisAndNonEvmIsNotProbed(t *testing.T) {
	resetGockForAnalyzer(t)
	full := &analyzerNode{chainId: "0x1", latest: "0x10", blockHash: "0xh"}
	svm := &analyzerNode{}
	fullCfg := analyzerUpstream("full", startAnalyzerNode(t, full), 1)
	fullCfg.Evm.NodeType = common.EvmNodeTypeFull
	cfg := analyzerConfig(
		fullCfg,
		&common.UpstreamConfig{Id: "sol", Type: common.UpstreamTypeSvm, Endpoint: startAnalyzerNode(t, svm), Svm: &common.SvmUpstreamConfig{Chain: "mainnet-beta"}},
	)

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Empty(t, report.Errors)
	assert.Empty(t, report.Warnings)
	assert.Equal(t, []string{"project=main upstream=full skipped genesis hash check (full node or maxAvailableRecentBlocks configured)"},
		findingsWith(report.Notices, "skipped genesis"))
	assert.Zero(t, full.callCount("eth_getBlockByNumber(0x0)"))
	svm.mu.Lock()
	assert.Empty(t, svm.calls, "non-EVM upstreams receive no EVM probes")
	svm.mu.Unlock()
}

func TestConfigAnalyzerReport_ZeroChainIdIsWarned(t *testing.T) {
	resetGockForAnalyzer(t)
	n := &analyzerNode{chainId: "0x0", genesis: "0xg", latest: "0x10"}
	cfg := analyzerConfig(analyzerUpstream("zero", startAnalyzerNode(t, n), 0))

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Equal(t, []string{"In project 'main', upstream 'zero' returned chain ID 0 from eth_chainId, which is invalid."}, report.Warnings)
	assert.Empty(t, report.Errors)
}

// Upstreams whose chain id cannot be fetched are grouped by their configured
// chain id but never cross-compared.
func TestConfigAnalyzerReport_UnknownChainIdSkipsComparison(t *testing.T) {
	resetGockForAnalyzer(t)
	a := &analyzerNode{genesis: "0xaaa", latest: "0x5000"}
	b := &analyzerNode{genesis: "0xbbb", latest: "0x5000"}
	cfg := analyzerConfig(
		analyzerUpstream("a", startAnalyzerNode(t, a), 1),
		analyzerUpstream("b", startAnalyzerNode(t, b), 1),
	)

	report := GenerateValidationReport(context.Background(), cfg)

	assert.Empty(t, report.Errors, "differing genesis hashes on unknown chains are not errors")
	assert.Len(t, findingsWith(report.Warnings, "upstream 'a' could not fetch chain ID via eth_chainId"), 1, "%v", report.Warnings)
	assert.Len(t, findingsWith(report.Warnings, "upstream 'b' could not fetch chain ID via eth_chainId"), 1, "%v", report.Warnings)
	assert.Equal(t, []string{"chain=cfg:1 project=main skipping block hash comparison (chain ID unknown for all upstreams)"},
		findingsWith(report.Warnings, "skipping block hash comparison"))
	assert.Len(t, report.Warnings, 3)
}

func TestConfigAnalyzerReport_MissingBlocksAreWarned(t *testing.T) {
	resetGockForAnalyzer(t)
	t.Run("no genesis and no heights", func(t *testing.T) {
		a := &analyzerNode{chainId: "0x1"}
		b := &analyzerNode{chainId: "0x1"}
		cfg := analyzerConfig(
			analyzerUpstream("a", startAnalyzerNode(t, a), 1),
			analyzerUpstream("b", startAnalyzerNode(t, b), 1),
		)
		report := GenerateValidationReport(context.Background(), cfg)
		assert.Empty(t, report.Errors)
		assert.ElementsMatch(t, []string{
			"chain=1 project=main no upstream returned genesis block",
			"chain=1 project=main could not resolve latest or finalized block from any upstream",
		}, report.Warnings)
	})
	t.Run("one upstream lacks genesis and historical block", func(t *testing.T) {
		a := &analyzerNode{chainId: "0x1", genesis: "0xg", finalized: "0x1000", blockHash: "0xh"}
		b := &analyzerNode{chainId: "0x1", finalized: "0x1000"}
		cfg := analyzerConfig(
			analyzerUpstream("a", startAnalyzerNode(t, a), 1),
			analyzerUpstream("b", startAnalyzerNode(t, b), 1),
		)
		report := GenerateValidationReport(context.Background(), cfg)
		assert.Empty(t, report.Errors)
		assert.Len(t, report.Warnings, 2, "%v", report.Warnings)
		assert.Len(t, findingsWith(report.Warnings, "project=main upstream=b chain=1 could not fetch genesis block:"), 1)
		assert.Len(t, findingsWith(report.Warnings, "project=main upstream=b chain=1 could not fetch block 4032:"), 1)
	})
	t.Run("no upstream has the historical block", func(t *testing.T) {
		a := &analyzerNode{chainId: "0x1", genesis: "0xg", finalized: "0x1000"}
		b := &analyzerNode{chainId: "0x1", genesis: "0xg", finalized: "0x1000"}
		cfg := analyzerConfig(
			analyzerUpstream("a", startAnalyzerNode(t, a), 1),
			analyzerUpstream("b", startAnalyzerNode(t, b), 1),
		)
		report := GenerateValidationReport(context.Background(), cfg)
		assert.Empty(t, report.Errors)
		assert.Equal(t, []string{"chain=1 project=main could not fetch block 4032 from any upstream"}, report.Warnings)
	})
}

func TestConfigAnalyzerReport_Renderers(t *testing.T) {
	r := &ValidationReport{
		Errors:   []string{"broken"},
		Warnings: []string{"careful"},
		Notices:  []string{"fyi"},
		Resources: ValidationResources{
			Totals: ValidationTotals{ProjectsTotal: 1, NetworksTotal: 1, UpstreamsTotal: 2, RateLimitBudgetsTotal: 1},
			Tree: ValidationResourcesTree{
				Projects: []ProjectNode{{Id: "main", Networks: []NetworkNode{
					{Id: "evm:1", Alias: "mainnet", Upstreams: []UpstreamNode{{Id: "u1"}}},
					{Id: "evm:10", Upstreams: []UpstreamNode{{Id: "u2"}}},
				}}},
				RateLimiters: RateLimitersNode{Budgets: []RateLimitBudgetNode{{Id: "b1", RulesCount: 3}}},
			},
		},
	}

	md := RenderValidationReportMarkdown(r)
	for _, want := range []string{
		"### Errors\n- ❌ broken\n",
		"<summary>Warnings (1)</summary>", "- ⚠️ careful\n",
		"<summary>Notices (1)</summary>", "- 💡 fyi\n",
		"- projectsTotal: 1\n", "- upstreamsTotal: 2\n", "- rateLimitBudgetsTotal: 1\n",
		"project main\n  network evm:1 (mainnet)\n    upstream u1\n  network evm:10\n    upstream u2\n",
		"rateLimiters\n  budget b1 rules=3\n",
	} {
		assert.Contains(t, md, want)
	}

	empty := RenderValidationReportMarkdown(&ValidationReport{})
	assert.NotContains(t, empty, "Errors")
	assert.NotContains(t, empty, "Warnings")
	assert.NotContains(t, empty, "Notices")
	assert.Contains(t, empty, "- projectsTotal: 0\n")

	compact, err := RenderValidationReportJSON(r, false)
	require.NoError(t, err)
	pretty, err := RenderValidationReportJSON(r, true)
	require.NoError(t, err)
	assert.NotContains(t, compact, "\n")
	assert.Contains(t, pretty, "\n  \"errors\": [")
	var a, b map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(compact), &a))
	require.NoError(t, json.Unmarshal([]byte(pretty), &b))
	assert.Equal(t, a, b)
	assert.Equal(t, "mainnet", a["resources"].(map[string]interface{})["tree"].(map[string]interface{})["projects"].([]interface{})[0].(map[string]interface{})["networks"].([]interface{})[0].(map[string]interface{})["alias"])
}

func TestConfigAnalyzerStats_UsageAndOrphans(t *testing.T) {
	cfg := &common.Config{
		RateLimiters: &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{
			{Id: "p"}, {Id: "u"}, {Id: "n"}, {Id: "auth"}, {Id: "admin"}, {Id: "orphan"},
		}},
		Admin: &common.AdminConfig{Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, RateLimitBudget: "admin"},
		}}},
		Projects: []*common.ProjectConfig{{
			Id:              "main",
			RateLimitBudget: "p",
			Upstreams: []*common.UpstreamConfig{
				{Id: "named", RateLimitBudget: "u"},
				{Type: common.UpstreamTypeEvm, VendorName: "alchemy"},
				{Type: common.UpstreamTypeEvm},
			},
			Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, RateLimitBudget: "n"}},
			Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeJwt, RateLimitBudget: "auth"},
			}},
		}},
	}

	stats := calculateConfigStats(cfg)

	assert.Equal(t, 1, stats.ProjectCount)
	assert.Equal(t, 6, stats.RateLimits.TotalBudgets)
	assert.Equal(t, []string{"orphan"}, stats.RateLimits.OrphanBudgets)
	assert.Equal(t, map[string][]string{
		"p":     {"project:main"},
		"u":     {"project:main:upstream:named"},
		"n":     {"project:main:network:evm"},
		"auth":  {"project:main:auth:jwt"},
		"admin": {"admin:auth:secret"},
	}, stats.RateLimits.UsageBySources)
	require.Len(t, stats.Projects, 1)
	assert.Equal(t, ProjectStats{
		ProjectID:     "main",
		UpstreamCount: 3,
		Upstreams:     []string{"named", "evm-alchemy", "evm-unknown"},
		NetworkCount:  1,
		Networks:      []string{"evm"},
	}, stats.Projects[0])
}

func TestConfigAnalyzerIsLocalEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		local    bool
		wantErr  bool
	}{
		{"http://localhost:8545", true, false},
		{"http://127.0.0.1", true, false},
		{"http://[::1]:8545", true, false},
		{"http://node.default.svc.cluster.local", true, false},
		{"http://10.1.2.3", true, false},
		{"http://192.168.0.5:80", true, false},
		{"http://127.0.0.2", true, false},
		{"https://rpc.example.com", false, false},
		{"http://8.8.8.8", false, false},
		{"wss://", false, false},
		{"http://bad host/%zz", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			local, err := isLocalEndpoint(tc.endpoint)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.local, local)
		})
	}
}

func TestConfigAnalyzerAnalyseConfig(t *testing.T) {
	resetGockForAnalyzer(t)
	logger := zerolog.New(io.Discard)
	nodeURL := func(chainId string) string {
		return startAnalyzerNode(t, &analyzerNode{chainId: chainId})
	}
	cases := []struct {
		name      string
		mutate    func(cfg *common.Config)
		ignoreEnv string
		wantErr   string
	}{
		{name: "upstream without chain id is skipped", mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{{Id: "a", Endpoint: "http://unreachable.invalid"}}
		}},
		{name: "empty endpoint", wantErr: `upstream endpoint is empty for project: "main" and upstream id: "a"`, mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{{Id: "a"}}
		}},
		{name: "invalid histogram buckets", wantErr: "failed to set histogram buckets", mutate: func(cfg *common.Config) {
			cfg.Metrics.HistogramBuckets = "x"
		}},
		{name: "proxy pool without urls", wantErr: `failed to create proxy pool registry for project: "main"`, mutate: func(cfg *common.Config) {
			cfg.ProxyPools = []*common.ProxyPoolConfig{{ID: "p"}}
		}},
		{name: "chain id unavailable", wantErr: `failed to get chain id for project: "main" and upstream id: "a"`, mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{analyzerUpstream("a", nodeURL(""), 1)}
		}},
		{name: "local endpoint skipped when ignored", ignoreEnv: "true", mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{analyzerUpstream("a", nodeURL("0x5"), 1)}
		}},
		{name: "non-http endpoint skipped when ignored", ignoreEnv: "true", mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{analyzerUpstream("a", "alchemy://key", 1)}
		}},
		{name: "unparseable endpoint when ignoring locals", ignoreEnv: "true", wantErr: `invalid endpoint URL for project "main" upstream "a"`, mutate: func(cfg *common.Config) {
			cfg.Projects[0].Upstreams = []*common.UpstreamConfig{analyzerUpstream("a", "http://bad host/%zz", 1)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION", tc.ignoreEnv)
			cfg := analyzerConfig()
			cfg.RateLimiters = &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{{Id: "unused"}}}
			tc.mutate(cfg)
			err := AnalyseConfig(context.Background(), cfg, logger)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
