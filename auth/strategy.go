package auth

import (
	"context"

	"github.com/erpc/erpc/common"
)

type AuthStrategy interface {
	Supports(ap *AuthPayload) bool
	Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error)
}
