package common

import (
	"context"
	"fmt"
	"strconv"
)

// MatcherAction declares whether a matched MatcherConfig opts the
// associated policy IN or OUT for the request.
type MatcherAction string

const (
	MatcherInclude MatcherAction = "include"
	MatcherExclude MatcherAction = "exclude"
)

// MatcherConfig is the unified request matcher shared by failsafe-policy
// selection (and, in time, any other subsystem that needs to scope a
// policy to a class of requests). It supersedes the scattered
// `matchMethod` / `matchFinality` fields with a single object model that
// can additionally constrain on network, request params, response
// emptiness, and an explicit include/exclude action.
//
// All set fields are AND-ed together; an unset field imposes no
// constraint. String fields (Network, Method, and string param patterns)
// are matched with the project's WildcardMatch grammar — globs, the
// `|`/`&`/`!` boolean operators, parenthesisation, `<empty>`, and numeric
// hex/decimal comparisons such as `>=0x100000`.
type MatcherConfig struct {
	// Network is a wildcard pattern matched against the request's network
	// id (e.g. "evm:1"). Empty = match any network.
	Network string `yaml:"network,omitempty" json:"network,omitempty"`
	// Method is a wildcard pattern matched against the JSON-RPC method.
	// Empty = match any method.
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	// Params is a positional list of patterns compared element-wise
	// against the request params (recursive for objects/arrays). Empty =
	// match any params. A shorter list constrains only the leading params;
	// trailing params are unconstrained.
	Params []interface{} `yaml:"params,omitempty" json:"params,omitempty"`
	// Finality is a set (OR) of finality states; the request matches if
	// its finality is any member. Empty = match any finality.
	Finality []DataFinalityState `yaml:"finality,omitempty" json:"finality,omitempty" tstype:"DataFinalityState[]"`
	// Empty is a tri-state response-emptiness constraint. It is a pointer
	// so "unset" (nil = no constraint) is distinguishable from the
	// zero-value CacheEmptyBehaviorIgnore. Only meaningful where a response
	// is available at match time (e.g. a cache SET); it is inert for
	// pre-forward failsafe selection (the response is nil there).
	Empty *CacheEmptyBehavior `yaml:"empty,omitempty" json:"empty,omitempty" tstype:"CacheEmptyBehavior"`
	// Action is "include" (default) or "exclude". Defaulted and validated
	// by ValidateMatchers.
	Action MatcherAction `yaml:"action,omitempty" json:"action,omitempty" tstype:"'include' | 'exclude'"`
}

// Copy returns a deep, independent clone. Mutating the clone (including
// through the Empty pointer) never affects the original.
func (m *MatcherConfig) Copy() *MatcherConfig {
	if m == nil {
		return nil
	}
	cp := *m
	if m.Params != nil {
		cp.Params = make([]interface{}, len(m.Params))
		copy(cp.Params, m.Params)
	}
	if m.Finality != nil {
		cp.Finality = make([]DataFinalityState, len(m.Finality))
		copy(cp.Finality, m.Finality)
	}
	if m.Empty != nil {
		e := *m.Empty
		cp.Empty = &e
	}
	return &cp
}

// CopyMatchers deep-copies a matcher slice (nil-safe), used by config
// Copy() implementations that embed []*MatcherConfig.
func CopyMatchers(matchers []*MatcherConfig) []*MatcherConfig {
	if matchers == nil {
		return nil
	}
	out := make([]*MatcherConfig, len(matchers))
	for i, m := range matchers {
		out[i] = m.Copy()
	}
	return out
}

// ValidateMatchers defaults each matcher's Action to MatcherInclude and
// rejects any action outside {include, exclude}. Matchers are mutated in
// place (action defaulting), so callers should run this once at config
// load before the slice is consulted on the hot path.
func ValidateMatchers(matchers []*MatcherConfig) error {
	for i, m := range matchers {
		if m == nil {
			return fmt.Errorf("matchers[%d] is nil", i)
		}
		switch m.Action {
		case "":
			m.Action = MatcherInclude
		case MatcherInclude, MatcherExclude:
			// ok
		default:
			return fmt.Errorf("matchers[%d].action must be 'include' or 'exclude', got %q", i, m.Action)
		}
		// Validate the wildcard patterns at load time so a typo (e.g. an
		// unbalanced paren or a malformed numeric comparison) fails loudly
		// instead of becoming a silently-dead rule that never matches.
		if m.Network != "" {
			if err := ValidatePattern(m.Network); err != nil {
				return fmt.Errorf("matchers[%d].network is not a valid pattern: %w", i, err)
			}
		}
		if m.Method != "" {
			if err := ValidatePattern(m.Method); err != nil {
				return fmt.Errorf("matchers[%d].method is not a valid pattern: %w", i, err)
			}
		}
		// Top-level string params use the same wildcard grammar; validate
		// them too. Nested object/array params are matched structurally and
		// their string leaves are not pre-validated here.
		for j, p := range m.Params {
			if s, ok := p.(string); ok && s != "" {
				if err := ValidatePattern(s); err != nil {
					return fmt.Errorf("matchers[%d].params[%d] is not a valid pattern: %w", i, j, err)
				}
			}
		}
	}
	return nil
}

// MatchConfig reports whether a single matcher matches the given request
// facets. Action is intentionally NOT considered here — callers
// (MatchMatchers, the cache loops) apply the include/exclude decision.
// All set fields must match (AND); unset fields impose no constraint.
func MatchConfig(config *MatcherConfig, networkId, method string, params []interface{}, finality DataFinalityState, isEmptyish bool) bool {
	if config == nil {
		return false
	}

	// Bare "*" (and "") impose no constraint — short-circuit before the
	// tokenizing WildcardMatch so the common catch-all case stays
	// allocation-free (mirrors the legacy selector's `== "*"` fast path).
	if config.Network != "" && config.Network != "*" {
		match, err := WildcardMatch(config.Network, networkId)
		if err != nil || !match {
			return false
		}
	}

	if config.Method != "" && config.Method != "*" {
		match, err := WildcardMatch(config.Method, method)
		if err != nil || !match {
			return false
		}
	}

	if len(config.Params) > 0 {
		match, err := MatchParams(config.Params, params)
		if err != nil || !match {
			return false
		}
	}

	if len(config.Finality) > 0 {
		matched := false
		for _, f := range config.Finality {
			if f == finality {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Empty is enforced only when explicitly set (non-nil pointer). A nil
	// Empty means "no constraint" — never filter on emptiness either way.
	if config.Empty != nil {
		if isEmptyish && *config.Empty == CacheEmptyBehaviorIgnore {
			return false
		}
		if !isEmptyish && *config.Empty == CacheEmptyBehaviorOnly {
			return false
		}
	}

	return true
}

// MatchMatchers evaluates a matcher slice against a request and returns
// the include/exclude decision. Matchers are evaluated last-to-first
// (the LAST matching rule wins, mirroring cache-policy precedence), and
// the result is whether that rule's Action is "include". An empty slice
// or a request that matches nothing returns false (excluded).
//
// `resp` may be nil when no response exists yet (the failsafe-selection
// case); the Empty constraint is then inert because isEmptyish is false.
func MatchMatchers(ctx context.Context, configs []*MatcherConfig, req *NormalizedRequest, resp *NormalizedResponse) bool {
	if len(configs) == 0 || req == nil {
		return false
	}

	method, _ := req.Method()
	finality := req.Finality(ctx)
	networkId := req.NetworkId()

	var params []interface{}
	if jrpcReq, err := req.JsonRpcRequest(); err == nil && jrpcReq != nil {
		jrpcReq.RLock()
		params = jrpcReq.Params
		jrpcReq.RUnlock()
	}

	isEmptyish := false
	if resp != nil {
		isEmptyish = resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx)
	}

	// Last-to-first: the last matching rule in the slice wins.
	for i := len(configs) - 1; i >= 0; i-- {
		c := configs[i]
		if c == nil {
			continue
		}
		if MatchConfig(c, networkId, method, params, finality, isEmptyish) {
			// Empty action defaults to include (ValidateMatchers normalizes
			// it at load; treat it as include here too so an un-normalized
			// slice still behaves predictably). Only an explicit "exclude"
			// opts out.
			return c.Action != MatcherExclude
		}
	}

	return false
}

// MatchParams reports whether the positional pattern list matches the
// request params. An empty pattern list matches anything. Element i of
// the pattern is matched against params[i]; a missing param is matched
// against nil. See MatchParam for per-element semantics.
func MatchParams(pattern []interface{}, params []interface{}) (bool, error) {
	if len(pattern) == 0 {
		return true, nil
	}
	for i, p := range pattern {
		var v interface{}
		if i < len(params) {
			v = params[i]
		}
		match, err := MatchParam(p, v)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

// MatchParam matches a single param element against a pattern element,
// dispatching on the pattern's Go type:
//   - map[string]interface{}: param must be a map; every pattern key must
//     exist in the param and match recursively (extra param keys allowed).
//   - []interface{}: param must be an array of exactly equal length;
//     element-wise recursive match.
//   - string: WildcardMatch(pattern, stringified-param) — the full glob /
//     boolean / numeric-comparison grammar applies.
//   - anything else: stringify both and compare for equality.
func MatchParam(pattern interface{}, param interface{}) (bool, error) {
	switch p := pattern.(type) {
	case map[string]interface{}:
		// For objects, every pattern key must exist in the param and match
		// recursively (extra param keys are allowed).
		paramMap, ok := param.(map[string]interface{})
		if !ok {
			return false, nil
		}
		for k, v := range p {
			paramValue, exists := paramMap[k]
			match, err := MatchParam(v, paramValue)
			if err != nil {
				return false, err
			}
			if !exists || !match {
				return false, nil
			}
		}
		return true, nil
	case []interface{}:
		paramArray, ok := param.([]interface{})
		if !ok || len(p) != len(paramArray) {
			return false, nil
		}
		for i, v := range p {
			match, err := MatchParam(v, paramArray[i])
			if err != nil {
				return false, err
			}
			if !match {
				return false, nil
			}
		}
		return true, nil
	case string:
		return WildcardMatch(p, ParamToString(param))
	default:
		return ParamToString(pattern) == ParamToString(param), nil
	}
}

// ParamToString renders a param value to the string form used for
// wildcard / equality matching.
func ParamToString(param interface{}) string {
	if param == nil {
		return ""
	}
	switch v := param.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
