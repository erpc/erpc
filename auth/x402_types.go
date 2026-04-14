package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// x402 protocol types and facilitator client, inlined to avoid heavy transitive
// dependencies from external x402 libraries (solana-go, mongodb, etc.).
// Supports both v1 (X-PAYMENT header) and v2 (Payment-Signature header, Circle Gateway).

// X402PaymentRequirement represents a single payment option in a 402 response.
type X402PaymentRequirement struct {
	Scheme            string                 `json:"scheme"`
	Network           string                 `json:"network"`
	MaxAmountRequired string                 `json:"maxAmountRequired,omitempty"`
	Amount            string                 `json:"amount,omitempty"`
	Asset             string                 `json:"asset"`
	PayTo             string                 `json:"payTo"`
	Resource          string                 `json:"resource,omitempty"`
	Description       string                 `json:"description,omitempty"`
	MimeType          string                 `json:"mimeType,omitempty"`
	MaxTimeoutSeconds int                    `json:"maxTimeoutSeconds"`
	Extra             map[string]interface{} `json:"extra,omitempty"`
}

// X402PaymentRequirementsResponse is the HTTP 402 response per the x402 spec.
// Sent both as the response body and base64-encoded in the PAYMENT-REQUIRED header.
type X402PaymentRequirementsResponse struct {
	X402Version int                      `json:"x402Version"`
	Error       string                   `json:"error"`
	Accepts     []X402PaymentRequirement `json:"accepts"`
	Resource    interface{}              `json:"resource,omitempty"`
}

// X402PaymentPayload is a signed payment sent by the client.
// V1 uses X-PAYMENT header, v2 uses Payment-Signature header.
type X402PaymentPayload struct {
	X402Version int         `json:"x402Version,omitempty"`
	Scheme      string      `json:"scheme,omitempty"`
	Network     string      `json:"network,omitempty"`
	Payload     interface{} `json:"payload,omitempty"`
	// V2 fields (Circle Gateway)
	Resource interface{} `json:"resource,omitempty"`
	Accepted interface{} `json:"accepted,omitempty"`
}

// X402SettlementResponse is the facilitator's response after settling a payment.
type X402SettlementResponse struct {
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	Transaction string `json:"transaction,omitempty"`
	Network     string `json:"network"`
	Payer       string `json:"payer"`
}

// X402VerifyResponse is the facilitator's response after verifying a payment.
type X402VerifyResponse struct {
	IsValid       bool   `json:"isValid"`
	InvalidReason string `json:"invalidReason,omitempty"`
	Payer         string `json:"payer"`
}

// X402SupportedKind describes a payment type supported by the facilitator.
type X402SupportedKind struct {
	X402Version int                    `json:"x402Version"`
	Scheme      string                 `json:"scheme"`
	Network     string                 `json:"network"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

// X402SupportedResponse is the facilitator's response listing supported payment types.
type X402SupportedResponse struct {
	Kinds []X402SupportedKind `json:"kinds"`
}

// x402FacilitatorRequest is the JSON body sent to the facilitator for verify/settle.
type x402FacilitatorRequest struct {
	X402Version         int                    `json:"x402Version"`
	PaymentPayload      interface{}            `json:"paymentPayload"`
	PaymentRequirements X402PaymentRequirement `json:"paymentRequirements"`
}

// X402FacilitatorClient communicates with an x402 facilitator for payment verification and settlement.
type X402FacilitatorClient struct {
	BaseURL    string
	HTTPClient *http.Client
	// CDP auth fields (optional — only for api.cdp.coinbase.com)
	cdpKeyID     string
	cdpKeySecret ed25519.PrivateKey
}

// NewX402FacilitatorClient creates a facilitator client, optionally with CDP JWT auth.
func NewX402FacilitatorClient(baseURL, cdpKeyID, cdpKeySecretB64 string) (*X402FacilitatorClient, error) {
	c := &X402FacilitatorClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		cdpKeyID:   cdpKeyID,
	}
	if cdpKeyID != "" && cdpKeySecretB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(cdpKeySecretB64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode CDP API key secret: %w", err)
		}
		// Ed25519 key: 64 bytes (32 seed + 32 public) or 32 seed
		if len(decoded) == 64 {
			c.cdpKeySecret = ed25519.NewKeyFromSeed(decoded[:32])
		} else if len(decoded) == 32 {
			c.cdpKeySecret = ed25519.NewKeyFromSeed(decoded)
		} else {
			return nil, fmt.Errorf("unexpected CDP key length: %d (expected 32 or 64)", len(decoded))
		}
	}
	return c, nil
}

// signRequest adds a CDP JWT Authorization header if credentials are configured.
func (c *X402FacilitatorClient) signRequest(req *http.Request) error {
	if c.cdpKeySecret == nil {
		return nil
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	now := time.Now().Unix()
	parsedURL, _ := url.Parse(c.BaseURL)
	uri := req.Method + " " + parsedURL.Host + req.URL.Path

	header := base64URLEncode([]byte(fmt.Sprintf(
		`{"alg":"EdDSA","typ":"JWT","kid":"%s","nonce":"%x"}`,
		c.cdpKeyID, nonce,
	)))
	payload := base64URLEncode([]byte(fmt.Sprintf(
		`{"sub":"%s","iss":"cdp","aud":["cdp_service"],"nbf":%d,"exp":%d,"uri":"%s"}`,
		c.cdpKeyID, now, now+120, uri,
	)))
	message := header + "." + payload
	sig := ed25519.Sign(c.cdpKeySecret, []byte(message))
	jwt := message + "." + base64URLEncode(sig)

	req.Header.Set("Authorization", "Bearer "+jwt)
	return nil
}

func base64URLEncode(data []byte) string {
	s := base64.RawURLEncoding.EncodeToString(data)
	return strings.TrimRight(s, "=")
}

// Supported fetches the payment types supported by the facilitator.
func (c *X402FacilitatorClient) Supported(ctx context.Context) (*X402SupportedResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/supported", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create supported request: %w", err)
	}
	if err := c.signRequest(httpReq); err != nil {
		return nil, fmt.Errorf("failed to sign supported request: %w", err)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("facilitator supported request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("facilitator supported returned status %d: %s", resp.StatusCode, string(body))
	}

	var supported X402SupportedResponse
	if err := json.NewDecoder(resp.Body).Decode(&supported); err != nil {
		return nil, fmt.Errorf("failed to decode supported response: %w", err)
	}

	return &supported, nil
}

func (c *X402FacilitatorClient) Verify(ctx context.Context, x402Version int, payment interface{}, requirement X402PaymentRequirement) (*X402VerifyResponse, error) {
	req := x402FacilitatorRequest{
		X402Version:         x402Version,
		PaymentPayload:      payment,
		PaymentRequirements: requirement,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal verify request: %w", err)
	}

	// Debug: log the verify request body (truncated) to diagnose CDP rejections.
	if len(data) > 2000 {
		fmt.Printf("[x402-debug] verify request body (truncated): %s...\n", string(data[:2000]))
	} else {
		fmt.Printf("[x402-debug] verify request body: %s\n", string(data))
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/verify", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create verify request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := c.signRequest(httpReq); err != nil {
		return nil, fmt.Errorf("failed to sign verify request: %w", err)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("facilitator verify request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// CDP returns 400 with a valid verify response body for invalid payloads.
	// Parse the body for both 200 and 400 status codes.
	var verifyResp X402VerifyResponse
	if err := json.Unmarshal(body, &verifyResp); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("facilitator verify returned status %d: %s", resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("failed to decode verify response: %w", err)
	}

	// If we got a parseable response with status >= 400, surface it as an invalid payment
	// rather than an HTTP error.
	if resp.StatusCode >= 400 && !verifyResp.IsValid {
		return &verifyResp, nil
	} else if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("facilitator verify returned status %d: %s", resp.StatusCode, string(body))
	}

	if verifyResp.Payer == "" {
		verifyResp.Payer = extractPayerFromRaw(payment)
	}

	return &verifyResp, nil
}

func (c *X402FacilitatorClient) Settle(ctx context.Context, x402Version int, payment interface{}, requirement X402PaymentRequirement) (*X402SettlementResponse, error) {
	req := x402FacilitatorRequest{
		X402Version:         x402Version,
		PaymentPayload:      payment,
		PaymentRequirements: requirement,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal settle request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/settle", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create settle request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := c.signRequest(httpReq); err != nil {
		return nil, fmt.Errorf("failed to sign settle request: %w", err)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("facilitator settle request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("facilitator settle returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("failed to read settle response body: %w", err)
	}

	var settleResp X402SettlementResponse
	if err := json.Unmarshal(body, &settleResp); err != nil {
		return nil, fmt.Errorf("failed to decode settle response: %w (body: %s)", err, string(body))
	}

	if !settleResp.Success {
		settleResp.ErrorReason = fmt.Sprintf("%s (raw: %s)", settleResp.ErrorReason, string(body))
	}

	return &settleResp, nil
}

// decodeX402Payment decodes a base64-encoded payment header (X-PAYMENT or Payment-Signature).
func decodeX402Payment(encoded string) (map[string]interface{}, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("failed to decode base64: %w", err)
		}
	}

	var payment map[string]interface{}
	if err := json.Unmarshal(decoded, &payment); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payment: %w", err)
	}

	return payment, nil
}

// findMatchingRequirement finds a payment requirement matching the payment's scheme and network.
// Supports both v1 (top-level scheme/network) and v2 (inside "accepted" object).
func findMatchingRequirement(payment map[string]interface{}, requirements []X402PaymentRequirement) (*X402PaymentRequirement, error) {
	scheme, _ := payment["scheme"].(string)
	network, _ := payment["network"].(string)

	// V2: scheme/network may be inside the "accepted" object
	if accepted, ok := payment["accepted"].(map[string]interface{}); ok {
		if s, ok := accepted["scheme"].(string); ok && s != "" {
			scheme = s
		}
		if n, ok := accepted["network"].(string); ok && n != "" {
			network = n
		}
	}

	for i := range requirements {
		if requirements[i].Scheme == scheme && requirements[i].Network == network {
			return &requirements[i], nil
		}
	}
	return nil, fmt.Errorf("no matching payment requirement for scheme=%q network=%q", scheme, network)
}

// extractPayerFromRaw attempts to get the payer address from a raw payment payload.
func extractPayerFromRaw(payment interface{}) string {
	paymentMap, ok := payment.(map[string]interface{})
	if !ok {
		return ""
	}

	// V2: payload is at top level
	if payloadMap, ok := paymentMap["payload"].(map[string]interface{}); ok {
		if authMap, ok := payloadMap["authorization"].(map[string]interface{}); ok {
			if from, ok := authMap["from"].(string); ok {
				return from
			}
		}
	}

	// V1: might also have authorization at top level
	if authMap, ok := paymentMap["authorization"].(map[string]interface{}); ok {
		if from, ok := authMap["from"].(string); ok {
			return from
		}
	}

	return ""
}

