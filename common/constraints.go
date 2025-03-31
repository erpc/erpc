package common

func ResolveMaxAllowedGetLogsRangeConstraints(n Network, up Upstream) int64 {
	// 1. Check upstream-level constraint
	if up != nil && up.Config().Constraints != nil && up.Config().Constraints.Evm != nil {
		if up.Config().Constraints.Evm.MaxAllowedGetLogsRange > 0 {
			return up.Config().Constraints.Evm.MaxAllowedGetLogsRange
		}
	}

	// 2. Fallback to network-level constraint
	if n != nil && n.Config() != nil && n.Config().Constraints != nil && n.Config().Constraints.Evm != nil {
		if n.Config().Constraints.Evm.MaxAllowedGetLogsRange > 0 {
			return n.Config().Constraints.Evm.MaxAllowedGetLogsRange
		}
	}

	// No constraint defined
	return 0
}
