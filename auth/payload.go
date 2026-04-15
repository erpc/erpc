package auth

import "github.com/erpc/erpc/common"

type AuthPayload struct {
	Method     string
	Type       common.AuthType
	Secret     *SecretPayload
	Jwt        *JwtPayload
	Siwe       *SiwePayload
	X402 *X402Payload
}

// This payload is used by both "secret" and "database" strategies
type SecretPayload struct {
	Value string
}

type JwtPayload struct {
	Token string
}

type SiwePayload struct {
	Signature string
	Message   string
}

// X402Payload carries the base64-encoded X-PAYMENT header value for x402 authentication.
type X402Payload struct {
	Payment    string
	RequestURL string // Full request URL, used for the 402 response resource field
}

// x402RequestURL safely returns the RequestURL from the X402 payload, or empty string if nil.
func (ap *AuthPayload) x402RequestURL() string {
	if ap.X402 != nil {
		return ap.X402.RequestURL
	}
	return ""
}
