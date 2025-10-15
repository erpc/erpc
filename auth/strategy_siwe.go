package auth

import (
	"context"
	"fmt"
	"strings"

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

func (s *SiweStrategy) Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error) {
	if ap.Siwe == nil {
		return nil, common.NewErrAuthUnauthorized("siwe", "missing SIWE payload")
	}

	// Parse the SIWE message
	message, err := siwe.ParseMessage(ap.Siwe.Message)
	if err != nil {
		return nil, common.NewErrAuthUnauthorized("siwe", fmt.Sprintf("failed to parse SIWE message: %s", err))
	}

	// Verify the signature
	if _, err := message.VerifyEIP191(ap.Siwe.Signature); err != nil {
		return nil, common.NewErrAuthUnauthorized("siwe", fmt.Sprintf("failed to verify SIWE signature: %s", err))
	}

	// Check if the domain is allowed
	if !s.isDomainAllowed(message.GetDomain()) {
		return nil, common.NewErrAuthUnauthorized("siwe", fmt.Sprintf("domain %s is not allowed", message.GetDomain()))
	}

	// Verify the message is not expired
	if ok, err := message.ValidNow(); !ok {
		return nil, common.NewErrAuthUnauthorized("siwe", fmt.Sprintf("SIWE message expired: %s", err))
	}

	user := &common.User{Id: strings.ToLower(message.GetAddress().String())}
	if s.cfg.RateLimitBudget != "" {
		user.RateLimitBudget = s.cfg.RateLimitBudget
	}
	return user, nil
}

func (s *SiweStrategy) isDomainAllowed(domain string) bool {
	for _, allowedDomain := range s.cfg.AllowedDomains {
		if domain == allowedDomain {
			return true
		}
	}

	return false
}
