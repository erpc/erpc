package common

import "context"

type ctxKey string

const (
	ctxKeyChainIdChecks ctxKey = "erpc.chainIdChecksEnabled"
)

// WithChainIdChecks returns a child context that enables or disables
// chainId detection/validation during upstream initialization.
func WithChainIdChecks(parent context.Context, enabled bool) context.Context {
	return context.WithValue(parent, ctxKeyChainIdChecks, enabled)
}

// IsChainIdChecksEnabled reports whether chainId detection/validation is enabled in this context.
func IsChainIdChecksEnabled(ctx context.Context) bool {
	v := ctx.Value(ctxKeyChainIdChecks)
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
