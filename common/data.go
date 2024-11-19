package common

import (
	"fmt"
	"strings"
)

type DataFinalityState int

const (
	// Finalized gets 0 intentionally so that when user has not specified finality,
	// it defaults to finalized, which is safest sane default for caching.
	DataFinalityStateFinalized DataFinalityState = iota
	DataFinalityStateUnfinalized
	DataFinalityStateUnknown
)

func (f DataFinalityState) String() string {
	return []string{"finalized", "unfinalized", "unknown"}[f]
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
	case "finalized":
		*f = DataFinalityStateFinalized
		return nil
	}

	return fmt.Errorf("invalid data finality state: %s", s)
}
