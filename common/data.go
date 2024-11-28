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
	case "unknown":
		*f = DataFinalityStateUnknown
		return nil
	case "unfinalized":
		*f = DataFinalityStateUnfinalized
		return nil
	case "realtime":
		*f = DataFinalityStateRealtime
		return nil
	case "finalized":
		*f = DataFinalityStateFinalized
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

func (b *CacheEmptyBehavior) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	switch strings.ToLower(s) {
	case "ignore":
		*b = CacheEmptyBehaviorIgnore
		return nil
	case "allow":
		*b = CacheEmptyBehaviorAllow
		return nil
	case "only":
		*b = CacheEmptyBehaviorOnly
		return nil
	}

	return fmt.Errorf("invalid cache empty behavior: %s", s)
}
