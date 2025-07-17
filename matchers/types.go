package matchers

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
)

// Core Interfaces

// Matcher defines the interface for matching logic
type Matcher interface {
	Match(ctx context.Context, input MatchInput) (bool, error)
	String() string // For debugging and logging
}

// MatchInput is a marker interface for type safety
type MatchInput interface {
	GetType() string
}

// Rule represents a matching rule with associated actions
type Rule interface {
	ID() string
	Priority() int
	Matcher() Matcher
	Actions() []Action
	Enabled() bool
}

// Action represents an action to execute when a rule matches
type Action interface {
	Execute(ctx context.Context, input MatchInput) error
	String() string
}

// RuleEngine evaluates rules in priority order
type RuleEngine interface {
	AddRule(rule Rule) error
	RemoveRule(id string) error
	UpdateRule(rule Rule) error
	Evaluate(ctx context.Context, input MatchInput) ([]Action, error)
	GetMatchingRules(ctx context.Context, input MatchInput) ([]Rule, error)
	GetAllRules() []Rule
}

// Input Types

// RequestMatchInput contains request-related data for matching
type RequestMatchInput struct {
	Request     *common.NormalizedRequest
	Method      string
	Params      []interface{}
	Finality    common.DataFinalityState
	Network     string
	BlockNumber int64
	BlockRef    string
	Headers     map[string]string
	Directives  *common.RequestDirectives
}

func (r *RequestMatchInput) GetType() string {
	return "request"
}

// UpstreamMatchInput contains upstream-related data for matching
type UpstreamMatchInput struct {
	Upstream common.Upstream
	Metrics  UpstreamMetrics
	Config   *common.UpstreamConfig
	Network  string
	Method   string
}

func (u *UpstreamMatchInput) GetType() string {
	return "upstream"
}

// UpstreamMetrics contains performance metrics for upstream evaluation
type UpstreamMetrics struct {
	ErrorRate         float64
	BlockHeadLag      int64
	P99ResponseTime   float64
	TotalRequests     int64
	RateLimitedCount  int64
	SuccessRate       float64
	AvgResponseTime   float64
	LastErrorTime     *time.Time
	ConsecutiveErrors int64
}

// NetworkMatchInput contains network-related data for matching
type NetworkMatchInput struct {
	NetworkID    string
	Architecture common.NetworkArchitecture
	ChainID      int64
	Method       string
	Request      *common.NormalizedRequest
}

func (n *NetworkMatchInput) GetType() string {
	return "network"
}

// Configuration Types

// MatcherConfig represents configuration for a matcher
type MatcherConfig struct {
	Type   string                 `yaml:"type" json:"type"`
	Config map[string]interface{} `yaml:"config,omitempty" json:"config,omitempty"`
}

// RuleConfig represents configuration for a rule
type RuleConfig struct {
	ID       string          `yaml:"id" json:"id"`
	Priority int             `yaml:"priority" json:"priority"`
	Enabled  *bool           `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Match    *MatcherConfig  `yaml:"match" json:"match"`
	Actions  []*ActionConfig `yaml:"actions" json:"actions"`
}

// ActionConfig represents configuration for an action
type ActionConfig struct {
	Type   string                 `yaml:"type" json:"type"`
	Config map[string]interface{} `yaml:"config,omitempty" json:"config,omitempty"`
}

// RoutingRulesConfig represents the routing rules configuration
type RoutingRulesConfig struct {
	Rules []*RuleConfig `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// Factory interfaces for creating matchers and actions
type MatcherFactory interface {
	CreateMatcher(config *MatcherConfig) (Matcher, error)
	GetSupportedTypes() []string
}

type ActionFactory interface {
	CreateAction(config *ActionConfig) (Action, error)
	GetSupportedTypes() []string
}
