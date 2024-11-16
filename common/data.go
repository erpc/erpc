package common

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

func (f *DataFinalityState) UnmarshalJSON(data []byte) error {
	var s string
	if err := SonicCfg.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "unknown":
		*f = DataFinalityStateUnknown
	case "unfinalized":
		*f = DataFinalityStateUnfinalized
	case "finalized":
		*f = DataFinalityStateFinalized
	}
	return nil
}
