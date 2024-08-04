package auth

import "context"

type AuthStrategy interface {
	Supports(ap *AuthPayload) bool
	Authenticate(ctx context.Context, ap *AuthPayload) error
}
