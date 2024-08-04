package auth

import (
	"fmt"

	"github.com/erpc/erpc/common"
)

type Authorizer struct {
	cfg      *common.AuthStrategyConfig
	strategy AuthStrategy
}

func NewAuthorizer(cfg *common.AuthStrategyConfig) (*Authorizer, error) {
	var strategy AuthStrategy
	switch cfg.Type {
	case common.AuthTypeSecret:
		strategy = NewSecretStrategy(cfg.Secret)
	// case common.AuthTypeJwt:
	// 	strategy = NewJwtStrategy(cfg.Jwt)
	// case common.AuthTypeSiwe:
	// 	strategy = NewSiweStrategy(cfg.Siwe)
	default:
		return nil, common.NewErrInvalidConfig(fmt.Sprintf("unknown auth strategy type: %s", cfg.Type))
	}

	return &Authorizer{cfg: cfg, strategy: strategy}, nil
}
