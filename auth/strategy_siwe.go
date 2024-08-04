package auth

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/spruceid/siwe-go"
)

type SiweStrategy struct {
	cfg *common.SiweStrategyConfig
}

var _ AuthStrategy = &SiweStrategy{}

func NewSiweStrategy(cfg *common.SiweStrategyConfig) *SiweStrategy {
	return &SiweStrategy{cfg: cfg}
}

func (s *SiweStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeSiwe
}

func (s *SiweStrategy) Authenticate(ctx context.Context, ap *AuthPayload) error {
	if ap.Siwe == nil {
		return common.NewErrAuthUnauthorized("siwe", fmt.Errorf("missing SIWE payload"))
	}

	// Parse the SIWE message
	message, err := siwe.ParseMessage(ap.Siwe.Message)
	if err != nil {
		return common.NewErrAuthUnauthorized("siwe", fmt.Errorf("failed to parse SIWE message: %w", err))
	}

	// Verify the signature
	if _, err := message.VerifyEIP191(ap.Siwe.Signature); err != nil {
		return common.NewErrAuthUnauthorized("siwe", fmt.Errorf("invalid signature: %w", err))
	}

	// Check if the domain is allowed
	if !s.isDomainAllowed(message.GetDomain()) {
		return common.NewErrAuthUnauthorized("siwe", fmt.Errorf("domain %s is not allowed", message.GetDomain()))
	}

	// Verify the message is not expired
	if ok, err := message.ValidNow(); !ok {
		return common.NewErrAuthUnauthorized("siwe", err)
	}

	return nil
}

func (s *SiweStrategy) isDomainAllowed(domain string) bool {
	for _, allowedDomain := range s.cfg.AllowedDomains {
		if domain == allowedDomain {
			return true
		}
	}

	return false
}
