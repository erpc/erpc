package common

import (
	"fmt"
	"strings"
)

type DataFinalityState int

const (
	// Finalized gets 0 intentionally so that when user has not specified finality,
	// it defaults to finalized, which is safest sane default for caching.
	// This attribute will be calculated based on extracted block number (from request and/or response)
	// and comparing to the upstream (one that returned the response) 'finalized' block (fetch via evm state poller).
	DataFinalityStateFinalized DataFinalityState = iota

	// When we CAN determine the block number, and it's after the upstream 'finalized' block, we consider the data unfinalized.
	DataFinalityStateUnfinalized

	// Certain methods points are meant to be realtime and updated with every new block (e.g. eth_gasPrice).
	// These can be cached with short TTLs to improve performance.
	DataFinalityStateRealtime

	// When we CANNOT determine the block number (e.g some trace by hash calls), we consider the data unknown.
	// Most often it is safe to cache this data for longer as they're access when block hash is provided directly.
	DataFinalityStateUnknown
)

func (f DataFinalityState) String() string {
	return []string{"finalized", "unfinalized", "realtime", "unknown"}[f]
}

func (f DataFinalityState) MarshalYAML() (interface{}, error) {
	return f.String(), nil
}

func (f DataFinalityState) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(f.String())
}

func (f *DataFinalityState) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	switch strings.ToLower(s) {
	case "finalized", "0":
		*f = DataFinalityStateFinalized
		return nil
	case "unfinalized", "1":
		*f = DataFinalityStateUnfinalized
		return nil
	case "realtime", "2":
		*f = DataFinalityStateRealtime
		return nil
	case "unknown", "3":
		*f = DataFinalityStateUnknown
		return nil
	}

	return fmt.Errorf("invalid data finality state: %s", s)
}

type CacheEmptyBehavior int

const (
	CacheEmptyBehaviorIgnore CacheEmptyBehavior = iota
	CacheEmptyBehaviorAllow
	CacheEmptyBehaviorOnly
)

func (b CacheEmptyBehavior) String() string {
	return []string{"ignore", "allow", "only"}[b]
}

func (b CacheEmptyBehavior) MarshalYAML() (interface{}, error) {
	return b.String(), nil
}

func (b *CacheEmptyBehavior) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	switch strings.ToLower(s) {
	case "ignore", "0":
		*b = CacheEmptyBehaviorIgnore
		return nil
	case "allow", "1":
		*b = CacheEmptyBehaviorAllow
		return nil
	case "only", "2":
		*b = CacheEmptyBehaviorOnly
		return nil
	}

	return fmt.Errorf("invalid cache empty behavior: %s", s)
}

// CachePolicyAppliesTo controls whether a cache policy applies to get, set, or both operations.
type CachePolicyAppliesTo string

const (
	CachePolicyAppliesToBoth CachePolicyAppliesTo = "both"
	CachePolicyAppliesToGet  CachePolicyAppliesTo = "get"
	CachePolicyAppliesToSet  CachePolicyAppliesTo = "set"
)

func (a CachePolicyAppliesTo) String() string {
	if a == "" {
		return string(CachePolicyAppliesToBoth)
	}
	return string(a)
}

func (a CachePolicyAppliesTo) MarshalYAML() (interface{}, error) {
	return a.String(), nil
}

func (a CachePolicyAppliesTo) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(a.String())
}

func (a *CachePolicyAppliesTo) UnmarshalJSON(b []byte) error {
	var s string
	if err := SonicCfg.Unmarshal(b, &s); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		*a = CachePolicyAppliesToBoth
		return nil
	case "get":
		*a = CachePolicyAppliesToGet
		return nil
	case "set":
		*a = CachePolicyAppliesToSet
		return nil
	}
	return fmt.Errorf("invalid cache policy appliesTo: %s", s)
}

func (a *CachePolicyAppliesTo) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		*a = CachePolicyAppliesToBoth
		return nil
	case "get":
		*a = CachePolicyAppliesToGet
		return nil
	case "set":
		*a = CachePolicyAppliesToSet
		return nil
	}
	return fmt.Errorf("invalid cache policy appliesTo: %s", s)
}
