package common

import (
	"fmt"
	"strings"
)

type DataFinalityState int

const (
	DataFinalityStateUnknown DataFinalityState = iota
	DataFinalityStateUnfinalized
	DataFinalityStateFinalized
)

func (f DataFinalityState) String() string {
	return []string{"unknown", "unfinalized", "finalized"}[f]
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
	case "unfinalized":
		*f = DataFinalityStateUnfinalized
	case "finalized":
		*f = DataFinalityStateFinalized
	}

	return fmt.Errorf("invalid data finality state: %s", s)
}
