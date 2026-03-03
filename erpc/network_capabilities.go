package erpc

import "github.com/erpc/erpc/common"

func (n *Network) requiredCapabilitiesForMethod(method string) []string {
	if n == nil || n.cfg == nil || n.cfg.Methods == nil || n.cfg.Methods.Definitions == nil {
		return nil
	}
	methodCfg, ok := n.cfg.Methods.Definitions[method]
	if !ok || methodCfg == nil {
		return nil
	}
	return common.NormalizeCapabilityTags(methodCfg.Requires)
}

func filterUpstreamsByRequiredCapabilities(upsList []common.Upstream, required []string) []common.Upstream {
	normalizedRequired := common.NormalizeCapabilityTags(required)
	if len(normalizedRequired) == 0 {
		return upsList
	}

	filtered := make([]common.Upstream, 0, len(upsList))
	for _, ups := range upsList {
		if ups == nil {
			continue
		}
		cfg := ups.Config()
		if cfg == nil {
			continue
		}
		if common.HasAllCapabilities(cfg.Capabilities, normalizedRequired) {
			filtered = append(filtered, ups)
		}
	}
	return filtered
}
