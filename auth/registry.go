package auth

import (
	"github.com/erpc/erpc/common"
)

type AuthRegistry struct {
	authorizers []*Authorizer
}

func NewAuthRegistry(cfg *common.AuthConfig) (*AuthRegistry, error) {
	r := &AuthRegistry{
		authorizers: make([]*Authorizer, 0),
	}

	for _, strategy := range cfg.Strategies {
		az, err := NewAuthorizer(strategy)
		if err != nil {
			return nil, err
		}
		r.Register(az)
	}

	return r, nil
}

func (r *AuthRegistry) Register(strategy *Authorizer) {
	r.authorizers = append(r.authorizers, strategy)
}
