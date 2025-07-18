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

// DataFinalityStateSlice provides custom YAML unmarshaling for []DataFinalityState
type DataFinalityStateSlice []DataFinalityState

func (f *DataFinalityStateSlice) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try to unmarshal as a single value first
	var single DataFinalityState
	if err := unmarshal(&single); err == nil {
		*f = DataFinalityStateSlice{single}
		return nil
	}

	// If that fails, try to unmarshal as an array
	var array []DataFinalityState
	if err := unmarshal(&array); err != nil {
		return err
	}

	*f = DataFinalityStateSlice(array)
	return nil
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
