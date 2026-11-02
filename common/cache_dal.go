package common

import (
	"context"
)

type CacheDAL interface {
	Set(ctx context.Context, req *NormalizedRequest, res *NormalizedResponse) error
	Get(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error)
	IsObjectNull() bool
}

// ResolveCacheableConsensusPolicies turns CacheConfig.CacheableConsensusPolicies
// into the runtime set used by Evm/Svm JsonRpcCache.Set and AllowsSelectorCache.
//
//   - nil/omitted or empty → nil (filter off; Forward keeps the historical
//     !selectorOn skip). Opening the selector cache path is opt-in: listing
//     policies is the operator's statement that those grades are safe to
//     serve to any later caller (cache keys omit policy+auth).
//   - non-empty → membership set (filter on; AllowsSelectorCache true)
func ResolveCacheableConsensusPolicies(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		out[n] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
