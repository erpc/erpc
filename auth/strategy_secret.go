package auth

import (
	"context"

	"github.com/erpc/erpc/common"
)

type SecretStrategy struct {
	cfg *common.SecretStrategyConfig
}

var _ AuthStrategy = &SecretStrategy{}

func NewSecretStrategy(cfg *common.SecretStrategyConfig) *SecretStrategy {
	return &SecretStrategy{cfg: cfg}
}

func (s *SecretStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeSecret
}

func (s *SecretStrategy) Authenticate(ctx context.Context, ap *AuthPayload) (*common.User, error) {
	if ap.Secret.Value != s.cfg.Value {
		return nil, common.NewErrAuthUnauthorized("secret", "invalid secret")
	}

	return &common.User{
		Id: s.cfg.Value,
	}, nil
}
