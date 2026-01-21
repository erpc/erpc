package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// isLocalEndpoint checks if the given endpoint URL points to a local or private network address
func isLocalEndpoint(endpoint string) (bool, error) {
	parsedURL, err := url.Parse(endpoint)
	if err != nil {
		return false, fmt.Errorf("failed to parse endpoint URL: %w", err)
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		return false, nil
	}

	// Check for localhost variants
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" {
		return true, nil
	}

	// Check for Kubernetes cluster-local domains
	if strings.HasSuffix(hostname, ".cluster.local") {
		return true, nil
	}

	// Parse IP address and check if it's in private ranges
	ip := net.ParseIP(hostname)
	if ip != nil {
		// Check for private IP ranges
		if ip.IsLoopback() || ip.IsPrivate() {
			return true, nil
		}
	}

	return false, nil
}

// Analyse a config object and print a few stats
func AnalyseConfig(ctx context.Context, cfg *common.Config, logger zerolog.Logger) error {
	if err := validateUpstreamEndpoints(ctx, cfg, logger); err != nil {
		return err
	}

	// Print configuration statistics
	stats := calculateConfigStats(cfg)

	printConfigStats(logger, stats)

	return nil
}

// Validation report types (simple strings for findings)
type ValidationReport struct {
	Errors    []string            `json:"errors"`
	Warnings  []string            `json:"warnings"`
	Notices   []string            `json:"notices"`
	Resources ValidationResources `json:"resources"`
}

type ValidationResources struct {
	Totals ValidationTotals        `json:"totals"`
	Tree   ValidationResourcesTree `json:"tree"`
}

type ValidationTotals struct {
	ProjectsTotal         int `json:"projectsTotal"`
	NetworksTotal         int `json:"networksTotal"`
	UpstreamsTotal        int `json:"upstreamsTotal"`
	RateLimitBudgetsTotal int `json:"rateLimitBudgetsTotal"`
}

type ValidationResourcesTree struct {
	Projects     []ProjectNode    `json:"projects"`
	RateLimiters RateLimitersNode `json:"rateLimiters"`
}

type ProjectNode struct {
	Id       string        `json:"id"`
	Networks []NetworkNode `json:"networks"`
}

type NetworkNode struct {
	Id        string         `json:"id"`
	Alias     string         `json:"alias,omitempty"`
	Upstreams []UpstreamNode `json:"upstreams"`
	chainId   int64          `json:"-"`
}

type UpstreamNode struct {
	Id string `json:"id"`
}

type RateLimitersNode struct {
	Budgets []RateLimitBudgetNode `json:"budgets"`
}

type RateLimitBudgetNode struct {
	Id         string `json:"id"`
	RulesCount int    `json:"rulesCount"`
}

// GenerateValidationReport performs static and upstream checks without emitting logs.
func GenerateValidationReport(ctx context.Context, cfg *common.Config) *ValidationReport {
	report := &ValidationReport{
		Errors:   []string{},
		Warnings: []string{},
		Notices:  []string{},
	}

	// Build resources tree and totals
	tree := ValidationResourcesTree{
		Projects:     make([]ProjectNode, 0, len(cfg.Projects)),
		RateLimiters: RateLimitersNode{Budgets: []RateLimitBudgetNode{}},
	}
	totals := ValidationTotals{}

	// Rate limiters tree
	if cfg.RateLimiters != nil {
		for _, b := range cfg.RateLimiters.Budgets {
			rulesCount := 0
			if b.Rules != nil {
				rulesCount = len(b.Rules)
			}
			tree.RateLimiters.Budgets = append(tree.RateLimiters.Budgets, RateLimitBudgetNode{
				Id: b.Id, RulesCount: rulesCount,
			})
		}
		totals.RateLimitBudgetsTotal = len(cfg.RateLimiters.Budgets)
	}

	// Track used rate limit budgets for orphan detection
	definedBudgets := map[string]struct{}{}
	usedBudgets := map[string]struct{}{}
	if cfg.RateLimiters != nil {
		for _, b := range cfg.RateLimiters.Budgets {
			definedBudgets[b.Id] = struct{}{}
		}
	}

	// Projects/networks/upstreams
	for _, p := range cfg.Projects {
		pNode := ProjectNode{Id: p.Id, Networks: []NetworkNode{}}
		totals.ProjectsTotal++

		// Project-level rate limit usage
		if p.RateLimitBudget != "" {
			usedBudgets[p.RateLimitBudget] = struct{}{}
		}

		// Networks
		if p.Networks != nil {
			for _, nw := range p.Networks {
				id := string(nw.Architecture)
				if nw.Evm != nil && nw.Evm.ChainId != 0 {
					id = fmt.Sprintf("%s:%d", nw.Architecture, nw.Evm.ChainId)
				}
				nNode := NetworkNode{Id: id, Alias: nw.Alias, Upstreams: []UpstreamNode{}}
				// Keep chainId internally to match upstreams into the network
				if nw.Evm != nil {
					nNode.chainId = nw.Evm.ChainId
				}
				totals.NetworksTotal++
				pNode.Networks = append(pNode.Networks, nNode)
			}
		}

		// Upstreams
		for _, ups := range p.Upstreams {
			totals.UpstreamsTotal++
			chainIdCfg := int64(0)
			if ups.Evm != nil {
				chainIdCfg = ups.Evm.ChainId
			}
			uNode := UpstreamNode{Id: ups.Id}
			// Attach upstream into corresponding network node by chain id if possible
			if chainIdCfg != 0 {
				// find matching network node by chain id
				attached := false
				for i := range pNode.Networks {
					if pNode.Networks[i].chainId == chainIdCfg {
						pNode.Networks[i].Upstreams = append(pNode.Networks[i].Upstreams, uNode)
						attached = true
						break
					}
				}
				if !attached && len(pNode.Networks) > 0 {
					// If no matching network found, attach to the first for visibility
					pNode.Networks[0].Upstreams = append(pNode.Networks[0].Upstreams, uNode)
				}
			} else if len(pNode.Networks) > 0 {
				pNode.Networks[0].Upstreams = append(pNode.Networks[0].Upstreams, uNode)
			}

			// Upstream rate limit usage and auto-tune notice
			if ups.RateLimitBudget != "" {
				usedBudgets[ups.RateLimitBudget] = struct{}{}
				if ups.RateLimitAutoTune == nil || ups.RateLimitAutoTune.Enabled == nil || !*ups.RateLimitAutoTune.Enabled {
					report.Notices = append(report.Notices, fmt.Sprintf("project=%s upstream=%s rateLimit budget '%s' defined but auto-tune disabled", p.Id, ups.Id, ups.RateLimitBudget))
				}
			}
		}

		for _, provider := range p.Providers {
			if len(provider.Overrides) > 0 {
				for _, override := range provider.Overrides {
					if override.RateLimitBudget != "" {
						usedBudgets[override.RateLimitBudget] = struct{}{}
					}
				}
			}
		}

		// Auth strategies rate limit usage
		if p.Auth != nil {
			for _, s := range p.Auth.Strategies {
				if s.RateLimitBudget != "" {
					usedBudgets[s.RateLimitBudget] = struct{}{}
				}
			}
		}

		tree.Projects = append(tree.Projects, pNode)

		// Notices: missing failsafe policies (neither defaults nor per-network)
		defaultsHasFailsafe := p.NetworkDefaults != nil && len(p.NetworkDefaults.Failsafe) > 0
		if p.Networks != nil {
			for _, nw := range p.Networks {
				hasNetworkFailsafe := len(nw.Failsafe) > 0
				if !defaultsHasFailsafe && !hasNetworkFailsafe {
					chainStr := "-"
					if nw.Evm != nil {
						chainStr = fmt.Sprintf("%d", nw.Evm.ChainId)
					}
					report.Notices = append(report.Notices, fmt.Sprintf("project=%s network=%s/%s has no failsafe policies (defaults and per-network empty)", p.Id, nw.Architecture, chainStr))
				}
			}
		}
	}

	// Admin auth budgets
	if cfg.Admin != nil && cfg.Admin.Auth != nil {
		for _, s := range cfg.Admin.Auth.Strategies {
			if s.RateLimitBudget != "" {
				usedBudgets[s.RateLimitBudget] = struct{}{}
			}
		}
	}

	// Orphan budgets notice
	for id := range definedBudgets {
		if _, ok := usedBudgets[id]; !ok {
			report.Notices = append(report.Notices, fmt.Sprintf("orphan rateLimit budget '%s'", id))
		}
	}

	report.Resources = ValidationResources{Totals: totals, Tree: tree}

	// Upstream runtime checks (chain id + block hash comparisons). Use a silent logger and short timeout per upstream
	silent := zerolog.New(io.Discard)

	// Histogram buckets (validate config value)
	if err := telemetry.SetHistogramBuckets(cfg.Metrics.HistogramBuckets); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("invalid metrics histogramBuckets: %v", err))
	}

	// Concurrency controls
	sem := make(chan struct{}, 50)
	var wg sync.WaitGroup
	var mu sync.Mutex

	appendErr := func(s string) { mu.Lock(); report.Errors = append(report.Errors, s); mu.Unlock() }
	appendWarn := func(s string) { mu.Lock(); report.Warnings = append(report.Warnings, s); mu.Unlock() }
	appendNote := func(s string) { mu.Lock(); report.Notices = append(report.Notices, s); mu.Unlock() }

	// Per-project, per-chain grouping for comparisons
	type checkItem struct {
		projectId      string
		upstreamId     string
		cfg            *common.UpstreamConfig
		ups            *upstream.Upstream
		chainKey       string
		genesisHash    string
		genesisTried   bool
		genesisErr     error
		genesisSkipped bool
		latestNum      int64
		latestOk       bool
	}

	groups := map[string][]*checkItem{}
	var gmu sync.Mutex

	for _, project := range cfg.Projects {
		var prxPool *clients.ProxyPoolRegistry
		var err error
		if cfg.ProxyPools != nil {
			prxPool, err = clients.NewProxyPoolRegistry(cfg.ProxyPools, &silent)
			if err != nil {
				appendErr(fmt.Sprintf("project=%s failed to create proxy pool registry: %v", project.Id, err))
				continue
			}
		}
		clReg := clients.NewClientRegistry(&silent, project.Id, prxPool, evm.NewJsonRpcErrorExtractor())
		vndReg := thirdparty.NewVendorsRegistry()
		rlr, err := upstream.NewRateLimitersRegistry(ctx, cfg.RateLimiters, &silent)
		if err != nil {
			appendErr(fmt.Sprintf("project=%s failed to create rate limiters registry: %v", project.Id, err))
			continue
		}
		mt := health.NewTracker(&silent, project.Id, time.Second*10)

		for _, upsCfg := range project.Upstreams {
			uc := upsCfg
			wg.Add(1)
			sem <- struct{}{}
			go func(prj string) {
				defer func() { <-sem; wg.Done() }()
				if strings.TrimSpace(uc.Endpoint) == "" {
					appendErr(fmt.Sprintf("In project '%s', upstream '%s' has an empty endpoint URL. Please set a valid endpoint.", prj, uc.Id))
					return
				}

				// Create upstream
				ups, err := upstream.NewUpstream(ctx, prj, uc, clReg, rlr, vndReg, &silent, mt, nil)
				if err != nil {
					appendErr(fmt.Sprintf("project=%s upstream=%s failed to create upstream: %v", prj, uc.Id, err))
					return
				}

				// Fetch chain id (do not abort on error)
				chainKey := "unknown"
				{
					cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					chainStr, cerr := ups.EvmGetChainId(cctx)
					cancel()
					if cerr != nil {
						appendWarn(fmt.Sprintf("In project '%s', upstream '%s' could not fetch chain ID via eth_chainId: %s.", prj, uc.Id, common.ErrorFingerprint(cerr)))
					} else if chainStr == "" {
						appendWarn(fmt.Sprintf("In project '%s', upstream '%s' returned an empty chain ID from eth_chainId.", prj, uc.Id))
					} else {
						cval, perr := strconv.ParseInt(chainStr, 0, 0)
						if perr != nil {
							appendWarn(fmt.Sprintf("In project '%s', upstream '%s' returned an invalid chain ID '%s' (parse error: %s).", prj, uc.Id, chainStr, common.ErrorFingerprint(perr)))
						} else {
							if cval == 0 {
								appendWarn(fmt.Sprintf("In project '%s', upstream '%s' returned chain ID 0 from eth_chainId, which is invalid.", prj, uc.Id))
							}
							chainKey = fmt.Sprintf("%d", cval)
							if uc.Evm != nil && uc.Evm.ChainId != 0 && cval != uc.Evm.ChainId {
								appendErr(fmt.Sprintf("In project '%s', upstream '%s' reported chain ID %d but the configuration expects %d.", prj, uc.Id, cval, uc.Evm.ChainId))
							}
						}
					}
				}

				it := &checkItem{projectId: prj, upstreamId: uc.Id, cfg: uc, ups: ups, chainKey: chainKey}

				// Genesis: skip for full nodes or when maxAvailableRecentBlocks is set
				if uc.Evm != nil && (uc.Evm.NodeType == common.EvmNodeTypeFull || uc.Evm.MaxAvailableRecentBlocks > 0) {
					it.genesisSkipped = true
					appendNote(fmt.Sprintf("project=%s upstream=%s skipped genesis hash check (full node or maxAvailableRecentBlocks configured)", prj, uc.Id))
				} else {
					it.genesisTried = true
					cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					h, herr := fetchBlockHashByNumber(cctx, ups, "0x0")
					cancel()
					if herr == nil && h != "" {
						it.genesisHash = h
					}
					it.genesisErr = herr
				}

				// Latest number
				{
					cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
					ln, lerr := fetchLatestNumber(cctx, ups)
					cancel()
					if lerr == nil && ln >= 0 {
						it.latestNum = ln
						it.latestOk = true
					}
				}

				// Store item into group
				gmu.Lock()
				gk := prj + "::" + it.chainKey
				groups[gk] = append(groups[gk], it)
				gmu.Unlock()
			}(project.Id)
		}
	}

	wg.Wait()

	// Compare genesis per group
	for gk, items := range groups {
		parts := strings.SplitN(gk, "::", 2)
		prj := parts[0]
		chainLabel := "unknown"
		if len(parts) == 2 && parts[1] != "" {
			chainLabel = parts[1]
		}

		baseline := ""
		anyAttempted := false
		for _, it := range items {
			if it.genesisSkipped {
				continue
			}
			if it.genesisTried {
				anyAttempted = true
			}
			if it.genesisHash != "" {
				baseline = it.genesisHash
				break
			}
		}
		if baseline == "" {
			if anyAttempted {
				appendWarn(fmt.Sprintf("chain=%s project=%s no upstream returned genesis block", chainLabel, prj))
			}
			continue
		}
		for _, it := range items {
			if it.genesisSkipped || it.genesisHash == "" {
				if it.genesisTried && it.genesisErr != nil {
					appendWarn(fmt.Sprintf("project=%s upstream=%s chain=%s could not fetch genesis block: %s", prj, it.upstreamId, chainLabel, common.ErrorFingerprint(it.genesisErr)))
				}
				continue
			}
			if !strings.EqualFold(it.genesisHash, baseline) {
				appendErr(fmt.Sprintf("project=%s upstream=%s chain=%s genesis hash mismatch expected=%s got=%s", prj, it.upstreamId, chainLabel, baseline, it.genesisHash))
			}
		}
	}

	// Historical comparison per group at latest-128
	targets := map[string]int64{}
	for gk, items := range groups {
		resolved := int64(-1)
		for _, it := range items {
			if it.latestOk {
				if resolved == -1 || it.latestNum < resolved {
					resolved = it.latestNum
				}
			}
		}
		parts := strings.SplitN(gk, "::", 2)
		prj := parts[0]
		chainLabel := "unknown"
		if len(parts) == 2 && parts[1] != "" {
			chainLabel = parts[1]
		}
		if resolved < 0 {
			appendWarn(fmt.Sprintf("chain=%s project=%s could not resolve latest block from any upstream", chainLabel, prj))
			continue
		}
		t := resolved - 128
		if t < 0 {
			t = 0
		}
		targets[gk] = t

		// Fetch hashes for target concurrently
		type hres struct {
			it   *checkItem
			hash string
			err  error
		}
		var hwg sync.WaitGroup
		semh := make(chan struct{}, 50)
		results := make([]hres, 0, len(items))
		var rmu sync.Mutex
		for _, it := range items {
			local := it
			hwg.Add(1)
			semh <- struct{}{}
			go func() {
				defer func() { <-semh; hwg.Done() }()
				tag := fmt.Sprintf("0x%x", t)
				cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				h, herr := fetchBlockHashByNumber(cctx, local.ups, tag)
				cancel()
				rmu.Lock()
				results = append(results, hres{it: local, hash: h, err: herr})
				rmu.Unlock()
			}()
		}
		hwg.Wait()

		baseline := ""
		for _, r := range results {
			if r.err == nil && r.hash != "" {
				baseline = r.hash
				break
			}
		}
		if baseline == "" {
			appendWarn(fmt.Sprintf("chain=%s project=%s could not fetch block %d from any upstream", chainLabel, prj, t))
			continue
		}
		for _, r := range results {
			if r.err != nil || r.hash == "" {
				appendWarn(fmt.Sprintf("project=%s upstream=%s chain=%s could not fetch block %d: %s", prj, r.it.upstreamId, chainLabel, t, common.ErrorFingerprint(r.err)))
				continue
			}
			if !strings.EqualFold(r.hash, baseline) {
				appendErr(fmt.Sprintf("project=%s upstream=%s chain=%s block=%d hash mismatch expected=%s got=%s", prj, r.it.upstreamId, chainLabel, t, baseline, r.hash))
			}
		}
	}

	return report
}

// Renderers
func RenderValidationReportJSON(r *ValidationReport, pretty bool) (string, error) {
	var b []byte
	var err error
	if pretty {
		b, err = json.MarshalIndent(r, "", "  ")
	} else {
		b, err = json.Marshal(r)
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func RenderValidationReportMarkdown(r *ValidationReport) string {
	var sb strings.Builder
	if len(r.Errors) > 0 {
		sb.WriteString("### Errors\n")
		for _, e := range r.Errors {
			sb.WriteString("- ❌ ")
			sb.WriteString(e)
			sb.WriteString("\n")
		}
	}
	if len(r.Warnings) > 0 {
		sb.WriteString("\n<details><summary>Warnings (" + strconv.Itoa(len(r.Warnings)) + ")</summary>\n\n")
		sb.WriteString("```\n")
		for _, w := range r.Warnings {
			sb.WriteString("- ⚠️ ")
			sb.WriteString(w)
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
		sb.WriteString("\n</details>\n")
	}
	if len(r.Notices) > 0 {
		sb.WriteString("\n<details><summary>Notices (" + strconv.Itoa(len(r.Notices)) + ")</summary>\n\n")
		sb.WriteString("```\n")
		for _, n := range r.Notices {
			sb.WriteString("- 💡 ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
		sb.WriteString("\n</details>\n")
	}
	sb.WriteString("\n### Resources\n")
	sb.WriteString(fmt.Sprintf("- projectsTotal: %d\n", r.Resources.Totals.ProjectsTotal))
	sb.WriteString(fmt.Sprintf("- networksTotal: %d\n", r.Resources.Totals.NetworksTotal))
	sb.WriteString(fmt.Sprintf("- upstreamsTotal: %d\n", r.Resources.Totals.UpstreamsTotal))
	sb.WriteString(fmt.Sprintf("- rateLimitBudgetsTotal: %d\n", r.Resources.Totals.RateLimitBudgetsTotal))

	// Collapsible tree
	sb.WriteString("\n<details><summary>Tree View</summary>\n\n")
	sb.WriteString("```\n")
	// Projects tree
	for _, p := range r.Resources.Tree.Projects {
		sb.WriteString(fmt.Sprintf("project %s\n", p.Id))
		for _, n := range p.Networks {
			label := n.Id
			if n.Alias != "" {
				label = fmt.Sprintf("%s (%s)", n.Id, n.Alias)
			}
			sb.WriteString(fmt.Sprintf("  network %s\n", label))
			for _, u := range n.Upstreams {
				sb.WriteString(fmt.Sprintf("    upstream %s\n", u.Id))
			}
		}
	}
	// Rate limiters
	sb.WriteString("rateLimiters\n")
	for _, b := range r.Resources.Tree.RateLimiters.Budgets {
		sb.WriteString(fmt.Sprintf("  budget %s rules=%d\n", b.Id, b.RulesCount))
	}
	sb.WriteString("```\n")
	sb.WriteString("\n</details>\n")

	return sb.String()
}

type ConfigStats struct {
	ProjectCount int
	Projects     []ProjectStats
	RateLimits   RateLimitStats
}

type ProjectStats struct {
	ProjectID     string
	UpstreamCount int
	Upstreams     []string // List of upstream IDs
	NetworkCount  int
	Networks      []string // List of network architectures
}

type RateLimitStats struct {
	TotalBudgets   int
	OrphanBudgets  []string            // List of unused rate limit budget IDs
	UsageBySources map[string][]string // Maps budget ID to where it's used
}

func calculateConfigStats(cfg *common.Config) ConfigStats {
	stats := ConfigStats{
		Projects: make([]ProjectStats, 0, len(cfg.Projects)),
		RateLimits: RateLimitStats{
			UsageBySources: make(map[string][]string),
		},
	}

	// Count projects and analyze each one
	stats.ProjectCount = len(cfg.Projects)

	// First, track all defined rate limit budgets
	definedBudgets := make(map[string]bool)
	if cfg.RateLimiters != nil {
		stats.RateLimits.TotalBudgets = len(cfg.RateLimiters.Budgets)
		for _, budget := range cfg.RateLimiters.Budgets {
			definedBudgets[budget.Id] = true
		}
	}

	// Analyze each project
	for _, project := range cfg.Projects {
		projectStats := ProjectStats{
			ProjectID: project.Id,
			Upstreams: make([]string, 0),
			Networks:  make([]string, 0),
		}

		// Track project-level rate limit usage
		if project.RateLimitBudget != "" {
			stats.RateLimits.UsageBySources[project.RateLimitBudget] = append(
				stats.RateLimits.UsageBySources[project.RateLimitBudget],
				fmt.Sprintf("project:%s", project.Id),
			)
		}

		// Analyze upstreams
		for _, upstream := range project.Upstreams {
			projectStats.UpstreamCount++
			if upstream.Id != "" {
				projectStats.Upstreams = append(projectStats.Upstreams, upstream.Id)
			} else {
				projectStats.Upstreams = append(projectStats.Upstreams,
					fmt.Sprintf("%s-%s", upstream.Type, upstream.Group))
			}

			if upstream.RateLimitBudget != "" {
				stats.RateLimits.UsageBySources[upstream.RateLimitBudget] = append(
					stats.RateLimits.UsageBySources[upstream.RateLimitBudget],
					fmt.Sprintf("project:%s:upstream:%s", project.Id, upstream.Id),
				)
			}
		}

		// Analyze networks
		if project.Networks != nil {
			for _, network := range project.Networks {
				projectStats.NetworkCount++
				projectStats.Networks = append(projectStats.Networks,
					string(network.Architecture))

				if network.RateLimitBudget != "" {
					stats.RateLimits.UsageBySources[network.RateLimitBudget] = append(
						stats.RateLimits.UsageBySources[network.RateLimitBudget],
						fmt.Sprintf("project:%s:network:%s", project.Id, network.Architecture),
					)
				}
			}
		}

		// Check auth strategies for rate limits
		if project.Auth != nil {
			for _, strategy := range project.Auth.Strategies {
				if strategy.RateLimitBudget != "" {
					stats.RateLimits.UsageBySources[strategy.RateLimitBudget] = append(
						stats.RateLimits.UsageBySources[strategy.RateLimitBudget],
						fmt.Sprintf("project:%s:auth:%s", project.Id, strategy.Type),
					)
				}
			}
		}

		stats.Projects = append(stats.Projects, projectStats)
	}

	// Check for global rate limits (admin section)
	if cfg.Admin != nil && cfg.Admin.Auth != nil {
		for _, strategy := range cfg.Admin.Auth.Strategies {
			if strategy.RateLimitBudget != "" {
				stats.RateLimits.UsageBySources[strategy.RateLimitBudget] = append(
					stats.RateLimits.UsageBySources[strategy.RateLimitBudget],
					fmt.Sprintf("admin:auth:%s", strategy.Type),
				)
			}
		}
	}

	// Find orphan budgets (defined but not used anywhere)
	for budgetID := range definedBudgets {
		if _, used := stats.RateLimits.UsageBySources[budgetID]; !used {
			stats.RateLimits.OrphanBudgets = append(stats.RateLimits.OrphanBudgets, budgetID)
		}
	}

	return stats
}

func printConfigStats(logger zerolog.Logger, stats ConfigStats) {
	logger.Info().Msg("Configuration Statistics:")

	// Print rate limit statistics
	rateLogger := logger.With().Str("component", "rate_limits").Logger()
	rateLogger.Info().Int("total", stats.RateLimits.TotalBudgets).Msg("Rate limit budgets defined")

	if len(stats.RateLimits.OrphanBudgets) > 0 {
		rateLogger.Warn().
			Strs("budgets", stats.RateLimits.OrphanBudgets).
			Msg("Found orphaned rate limit budgets")
	}

	// Print used rate limits and their locations
	for budgetID, usages := range stats.RateLimits.UsageBySources {
		rateLogger.Debug().
			Str("budget", budgetID).
			Strs("used_in", usages).
			Msg("Rate limit budget usage")
	}

	// Print project details
	logger.Info().Int("projects", stats.ProjectCount).Msg("Total projects")
	for _, project := range stats.Projects {
		projectLogger := logger.With().Str("project", project.ProjectID).Logger()

		projectLogger.Info().
			Int("upstreams", project.UpstreamCount).
			Int("networks", project.NetworkCount).
			Strs("upstream_ids", project.Upstreams).
			Strs("networks", project.Networks).
			Msg("Project details")
	}
}

func validateUpstreamEndpoints(ctx context.Context, cfg *common.Config, logger zerolog.Logger) error {
	err := telemetry.SetHistogramBuckets(
		cfg.Metrics.HistogramBuckets,
	)
	if err != nil {
		return fmt.Errorf("failed to set histogram buckets: %w", err)
	}
	for _, project := range cfg.Projects {
		var prxPool *clients.ProxyPoolRegistry
		var err error
		if cfg.ProxyPools != nil {
			prxPool, err = clients.NewProxyPoolRegistry(
				cfg.ProxyPools,
				&logger,
			)
		}
		if err != nil {
			return fmt.Errorf("failed to create proxy pool registry for project: \"%s\": %w", project.Id, err)
		}
		clReg := clients.NewClientRegistry(
			&logger,
			project.Id,
			prxPool,
			evm.NewJsonRpcErrorExtractor(),
		)
		vndReg := thirdparty.NewVendorsRegistry()
		rlr, err := upstream.NewRateLimitersRegistry(
			ctx,
			cfg.RateLimiters,
			&logger,
		)
		if err != nil {
			return fmt.Errorf("failed to create rate limiters registry for project: \"%s\": %w", project.Id, err)
		}
		mt := health.NewTracker(
			&logger,
			project.Id,
			time.Second*10,
		)
		for _, upsCfg := range project.Upstreams {
			if upsCfg.Endpoint == "" {
				return fmt.Errorf("upstream endpoint is empty for project: \"%s\" and upstream id: \"%s\"", project.Id, upsCfg.Id)
			}
			ignoreLocalEndpoints := os.Getenv("ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION") == "true"
			if ignoreLocalEndpoints {
				// Skip non-HTTP endpoints
				if !strings.HasPrefix(upsCfg.Endpoint, "http://") && !strings.HasPrefix(upsCfg.Endpoint, "https://") {
					continue
				}
				// Skip local endpoints using proper URL parsing
				isLocal, err := isLocalEndpoint(upsCfg.Endpoint)
				if err != nil {
					return fmt.Errorf("invalid endpoint URL for project \"%s\" upstream \"%s\": %w", project.Id, upsCfg.Id, err)
				}
				if isLocal {
					continue
				}
			}
			if upsCfg.Evm == nil || upsCfg.Evm.ChainId == 0 {
				logger.Warn().Str("project", project.Id).Str("upstream", upsCfg.Id).Msg("upstream has no chain id configured so will skip validation")
				continue
			}
			ups, err := upstream.NewUpstream(
				ctx,
				project.Id,
				upsCfg,
				clReg,
				rlr,
				vndReg,
				&logger,
				mt,
				nil,
			)
			if err != nil {
				return fmt.Errorf("failed to create upstream for project: \"%s\" and upstream id: \"%s\": %w", project.Id, upsCfg.Id, err)
			}
			chainStr, err := ups.EvmGetChainId(ctx)
			if err != nil {
				return fmt.Errorf("failed to get chain id for project: \"%s\" and upstream id: \"%s\": %w", project.Id, upsCfg.Id, err)
			}
			if chainStr == "" {
				return fmt.Errorf("chain id is nil for project: \"%s\" and upstream id: \"%s\"", project.Id, upsCfg.Id)
			}
			chain, err := common.HexToInt64(chainStr)
			if err != nil {
				return fmt.Errorf("failed to parse chain id for project: \"%s\" and upstream id: \"%s\": %w", project.Id, upsCfg.Id, err)
			}
			if chain == 0 {
				return fmt.Errorf("chain id is nil for project: \"%s\" and upstream id: \"%s\"", project.Id, upsCfg.Id)
			}
			if chain != upsCfg.Evm.ChainId {
				return fmt.Errorf("chain id mismatch for project: \"%s\" and upstream id: \"%s\": actual %d != configured %d", project.Id, upsCfg.Id, chain, upsCfg.Evm.ChainId)
			}
		}
	}

	return nil
}

// fetchBlockHashByNumber calls eth_getBlockByNumber with the given tag/hex number (e.g., "0x0", "latest")
// and returns the block hash as a string (e.g., "0xabc...").
func fetchBlockHashByNumber(ctx context.Context, ups *upstream.Upstream, blockTag string) (string, error) {
	if ups == nil {
		return "", fmt.Errorf("upstream is nil")
	}
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["%s",false]}`, util.RandomID(), blockTag),
	))
	resp, err := ups.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return "", err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr == nil {
		return "", fmt.Errorf("nil json-rpc response")
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	if jrr.IsResultEmptyish(ctx) {
		return "", fmt.Errorf("empty result")
	}
	hashStr, err := jrr.PeekStringByPath(ctx, "hash")
	if err != nil {
		return "", err
	}
	return hashStr, nil
}

// fetchLatestNumber resolves the latest block number via eth_getBlockByNumber("latest", false).
func fetchLatestNumber(ctx context.Context, ups *upstream.Upstream) (int64, error) {
	if ups == nil {
		return 0, fmt.Errorf("upstream is nil")
	}
	pr := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBlockByNumber","params":["latest",false]}`, util.RandomID()),
	))
	resp, err := ups.Forward(ctx, pr, true)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return 0, err
	}
	if jrr == nil {
		return 0, fmt.Errorf("nil json-rpc response")
	}
	if jrr.Error != nil {
		return 0, jrr.Error
	}
	if jrr.IsResultEmptyish(ctx) {
		return 0, fmt.Errorf("empty result")
	}
	numberStr, err := jrr.PeekStringByPath(ctx, "number")
	if err != nil {
		return 0, &common.BaseError{
			Code:    "ErrConfigAnalyzer",
			Message: "cannot get block number from block data",
			Details: map[string]interface{}{
				"blockTag": "latest",
				"result":   jrr.GetResultString(),
			},
		}
	}
	// Ensure string copy to detach from underlying buffers
	numberStr = string(append([]byte(nil), numberStr...))
	blockNum, err := common.HexToInt64(numberStr)
	if err != nil {
		return 0, err
	}
	return blockNum, nil
}
