package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
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

	// Merge config-level extra fields (e.g. EIP-712 domain params) into the requirement.
	// These serve as defaults; facilitator-provided values will override them below.
	if len(cfg.Extra) > 0 {
		if requirement.Extra == nil {
			requirement.Extra = make(map[string]interface{})
		}
		for k, v := range cfg.Extra {
			requirement.Extra[k] = v
		}
	}

	facilitator := NewX402FacilitatorClient(strings.TrimRight(cfg.FacilitatorURL, "/"))

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
			// If the facilitator doesn't list our exact scheme but reports a
			// higher version for our network, adopt that version.
			if kind.Network == requirement.Network && kind.X402Version > x402Version {
				x402Version = kind.X402Version
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
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse(ap.x402RequestURL()))
	}

	payment, err := decodeX402Payment(ap.X402.Payment)
	if err != nil {
		s.logger.Debug().Err(err).Msg("failed to decode x402 payment header")
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse(ap.x402RequestURL()))
	}

	matchedRequirement, err := findMatchingRequirement(payment, s.requirements)
	if err != nil {
		s.logger.Debug().Err(err).Msg("no matching x402 payment requirement for provided scheme/network")
		return nil, common.NewErrPaymentRequired(s.paymentRequirementsResponse(ap.x402RequestURL()))
	}

	// Resolve metric labels from the request context.
	project, network, facilitator := s.metricLabels(req)

	if s.cfg.VerifyOnly {
		// VerifyOnly mode: call verify for testing/dry-run without collecting payment.
		return s.authenticateWithVerify(ctx, payment, matchedRequirement, project, network, facilitator)
	}

	return s.authenticateWithSettle(ctx, payment, matchedRequirement, project, network, facilitator)
}

// settlePayment calls the facilitator settle endpoint and emits metrics.
func (s *X402Strategy) settlePayment(ctx context.Context, payment interface{}, req X402PaymentRequirement, project, network, facilitator string) error {
	settleStart := time.Now()
	settleResp, err := s.facilitator.Settle(ctx, s.x402Version, payment, req)
	settleDur := time.Since(settleStart).Seconds()
	if err != nil {
		telemetry.ObserverHandle(telemetry.MetricX402FacilitatorRequestDuration, project, network, facilitator, "settle", "error").Observe(settleDur)
		telemetry.CounterHandle(telemetry.MetricX402FacilitatorRequestTotal, project, network, facilitator, "settle", "error").Inc()
		telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "settle_error").Inc()
		return fmt.Errorf("settlement request failed: %w", err)
	}
	telemetry.ObserverHandle(telemetry.MetricX402FacilitatorRequestDuration, project, network, facilitator, "settle", "ok").Observe(settleDur)
	telemetry.CounterHandle(telemetry.MetricX402FacilitatorRequestTotal, project, network, facilitator, "settle", "ok").Inc()

	if !settleResp.Success {
		telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "settle_rejected").Inc()
		return fmt.Errorf("settlement rejected: %s", settleResp.ErrorReason)
	}
	telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "settled").Inc()
	return nil
}

// authenticateWithSettle settles payment immediately during auth (used for "exact" scheme).
func (s *X402Strategy) authenticateWithSettle(ctx context.Context, payment interface{}, req *X402PaymentRequirement, project, network, facilitator string) (*common.User, error) {
	if err := s.settlePayment(ctx, payment, *req, project, network, facilitator); err != nil {
		s.logger.Warn().Err(err).Msg("x402 exact payment settlement failed")
		return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment failed: %v", err))
	}

	// Extract payer from the settlement — we can get it from the raw payload
	// since we skipped verify.
	payer := extractPayerFromRaw(payment)
	if payer == "" {
		payer = "x402-unknown"
	}

	user := &common.User{Id: strings.ToLower(payer)}
	if s.cfg.RateLimitBudget != "" {
		user.RateLimitBudget = s.cfg.RateLimitBudget
	}

	s.logger.Debug().Str("payer", user.Id).Msg("x402 exact payment settled")
	return user, nil
}

// authenticateWithVerify uses the verify endpoint for VerifyOnly/dry-run mode.
func (s *X402Strategy) authenticateWithVerify(ctx context.Context, payment interface{}, req *X402PaymentRequirement, project, network, facilitator string) (*common.User, error) {
	verifyStart := time.Now()
	verifyResp, err := s.facilitator.Verify(ctx, s.x402Version, payment, *req)
	verifyDur := time.Since(verifyStart).Seconds()
	if err != nil {
		telemetry.ObserverHandle(telemetry.MetricX402FacilitatorRequestDuration, project, network, facilitator, "verify", "error").Observe(verifyDur)
		telemetry.CounterHandle(telemetry.MetricX402FacilitatorRequestTotal, project, network, facilitator, "verify", "error").Inc()
		telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "verify_error").Inc()
		s.logger.Warn().Err(err).Msg("x402 payment verification failed")
		return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment verification failed: %v", err))
	}
	telemetry.ObserverHandle(telemetry.MetricX402FacilitatorRequestDuration, project, network, facilitator, "verify", "ok").Observe(verifyDur)
	telemetry.CounterHandle(telemetry.MetricX402FacilitatorRequestTotal, project, network, facilitator, "verify", "ok").Inc()

	if !verifyResp.IsValid {
		telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "verify_invalid").Inc()
		s.logger.Debug().Str("reason", verifyResp.InvalidReason).Msg("x402 payment invalid")
		return nil, common.NewErrAuthUnauthorized("x402", fmt.Sprintf("payment invalid: %s", verifyResp.InvalidReason))
	}
	telemetry.CounterHandle(telemetry.MetricX402PaymentTotal, project, network, facilitator, "verify_valid").Inc()

	payer := verifyResp.Payer
	if payer == "" {
		payer = "x402-unknown"
	}
	user := &common.User{Id: strings.ToLower(payer)}
	if s.cfg.RateLimitBudget != "" {
		user.RateLimitBudget = s.cfg.RateLimitBudget
	}
	s.logger.Debug().Str("payer", user.Id).Msg("x402 payment verified (verify-only mode)")
	return user, nil
}

// metricLabels resolves project, network, and facilitator labels for metrics.
func (s *X402Strategy) metricLabels(req *common.NormalizedRequest) (project, network, facilitator string) {
	project = "n/a"
	network = s.cfg.Network
	facilitator = s.facilitatorLabel()
	if req != nil {
		if n := req.Network(); n != nil {
			project = n.ProjectId()
			network = req.NetworkLabel()
		}
	}
	return
}

// facilitatorLabel returns a short label for the facilitator URL (e.g. "circle", "x402org").
func (s *X402Strategy) facilitatorLabel() string {
	url := s.facilitator.BaseURL
	if strings.Contains(url, "x402.org") {
		return "x402org"
	}
	if strings.Contains(url, "circle") {
		return "circle"
	}
	// Fallback: extract hostname
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return "unknown"
}

func (s *X402Strategy) paymentRequirementsResponse(requestURL string) X402PaymentRequirementsResponse {
	requirements := make([]X402PaymentRequirement, len(s.requirements))
	copy(requirements, s.requirements)

	resp := X402PaymentRequirementsResponse{
		X402Version: s.x402Version,
		Error:       "Payment required for this resource",
		Accepts:     requirements,
	}

	if requestURL != "" {
		desc := s.cfg.Description
		if desc == "" {
			desc = "eRPC x402 endpoint"
		}
		resp.Resource = map[string]string{
			"url":         requestURL,
			"mimeType":    "application/json",
			"description": desc,
		}
	}

	return resp
}
