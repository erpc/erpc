package main

import (
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// Analyse a config object and print a few stats
func AnalyseConfig(cfg *common.Config, logger zerolog.Logger) error {
	// Print configuration statistics
	stats := calculateConfigStats(cfg)
	printConfigStats(logger, stats)

	return nil
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
