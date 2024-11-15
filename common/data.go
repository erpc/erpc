package common

type DataFinalityState string

const (
	DataFinalityStateUnknown     DataFinalityState = "unknown"
	DataFinalityStateUnfinalized DataFinalityState = "unfinalized"
	DataFinalityStateFinalized   DataFinalityState = "finalized"
)
