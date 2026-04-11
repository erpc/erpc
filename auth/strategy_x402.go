package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type X402Strategy struct {
	logger       *zerolog.Logger
	cfg          *common.X402StrategyConfig
	facilitator  *X402FacilitatorClient
	requirements []X402PaymentRequirement
	x402Version  int
}

var _ AuthStrategy = &X402Strategy{}

func NewX402Strategy(logger *zerolog.Logger, cfg *common.X402StrategyConfig) (*X402Strategy, error) {
	if cfg.FacilitatorURL == "" {
		return nil, fmt.Errorf("x402 strategy requires facilitatorUrl")
	}
	if cfg.SellerAddress == "" {
		return nil, fmt.Errorf("x402 strategy requires sellerAddress")
	}
	if cfg.PricePerRequest == "" {
		return nil, fmt.Errorf("x402 strategy requires pricePerRequest")
	}
	if cfg.Network == "" {
		return nil, fmt.Errorf("x402 strategy requires network")
	}

	scheme := cfg.Scheme
	if scheme == "" {
		scheme = "exact"
	}

	maxTimeout := cfg.MaxTimeoutSeconds
	if maxTimeout == 0 {
		maxTimeout = 300
	}

	requirement := X402PaymentRequirement{
		Scheme:            scheme,
		Network:           cfg.Network,
		MaxAmountRequired: cfg.PricePerRequest,
		Amount:            cfg.PricePerRequest,
		Asset:             cfg.Asset,
		PayTo:             cfg.SellerAddress,
		Description:       cfg.Description,
		MaxTimeoutSeconds: maxTimeout,
	}

	facilitator := &X402FacilitatorClient{
		BaseURL: strings.TrimRight(cfg.FacilitatorURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	x402Version := 1

	// Fetch supported payment kinds from the facilitator to get extra fields
	// (e.g. Circle Gateway's verifyingContract, name, version).
	supported, err := facilitator.Supported(context.Background())
	if err != nil {
		logger.Warn().Err(err).Msg("failed to fetch x402 supported kinds from facilitator, using defaults")
	} else {
		for _, kind := range supported.Kinds {
			if kind.Scheme == requirement.Scheme && kind.Network == requirement.Network {
				if kind.X402Version > x402Version {
					x402Version = kind.X402Version
				}
				if kind.Extra != nil {
					if requirement.Extra == nil {
						requirement.Extra = make(map[string]interface{})
					}
					for k, v := range kind.Extra {
						requirement.Extra[k] = v
					}
				}
				break
			}
		}
	}

	return &X402Strategy{
		logger:       logger,
		cfg:          cfg,
		facilitator:  facilitator,
		requirements: []X402PaymentRequirement{requirement},
		x402Version:  x402Version,
	}, nil
}

// Supports returns true for x402 payloads (X-PAYMENT or Payment-Signature header present)
// and for network-type payloads (no auth headers). The latter allows the strategy to
// return 402 Payment Required for unauthenticated requests.
func (s *X402Strategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeX402 || ap.Type == common.AuthTypeNetwork
}

func (s *X402Strategy) Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error) {
	if ap.X402 == nil || ap.X402.Payment == "" {
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse())
	}

	payment, err := decodeX402Payment(ap.X402.Payment)
	if err != nil {
		s.logger.Debug().Err(err).Msg("failed to decode x402 payment header")
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse())
	}

	matchedRequirement, err := findMatchingRequirement(payment, s.requirements)
	if err != nil {
		s.logger.Debug().Err(err).Msg("no matching x402 payment requirement for provided scheme/network")
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse())
	}

	// Pass the raw payment (map) to the facilitator — it handles both v1 and v2 formats
	verifyResp, err := s.facilitator.Verify(ctx, payment, *matchedRequirement)
	if err != nil {
		s.logger.Warn().Err(err).Msg("x402 payment verification failed")
		return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment verification failed: %v", err))
	}
	if !verifyResp.IsValid {
		s.logger.Debug().Str("reason", verifyResp.InvalidReason).Msg("x402 payment invalid")
		return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment invalid: %s", verifyResp.InvalidReason))
	}

	if !s.cfg.VerifyOnly {
		settleResp, err := s.facilitator.Settle(ctx, payment, *matchedRequirement)
		if err != nil {
			s.logger.Warn().Err(err).Msg("x402 payment settlement failed")
			return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment settlement failed: %v", err))
		}
		if !settleResp.Success {
			s.logger.Warn().Str("reason", settleResp.ErrorReason).Msg("x402 settlement unsuccessful")
			return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("settlement failed: %s", settleResp.ErrorReason))
		}
	}

	payer := verifyResp.Payer
	if payer == "" {
		payer = "x402-unknown"
	}

	user := &common.User{Id: strings.ToLower(payer)}
	if s.cfg.RateLimitBudget != "" {
		user.RateLimitBudget = s.cfg.RateLimitBudget
	}

	s.logger.Debug().Str("payer", user.Id).Msg("x402 payment authenticated")
	return user, nil
}

func (s *X402Strategy) paymentRequirementsResponse() X402PaymentRequirementsResponse {
	requirements := make([]X402PaymentRequirement, len(s.requirements))
	copy(requirements, s.requirements)

	return X402PaymentRequirementsResponse{
		X402Version: s.x402Version,
		Error:       "Payment required for this resource",
		Accepts:     requirements,
	}
}
